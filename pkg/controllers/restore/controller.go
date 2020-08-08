package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"k8s.io/apimachinery/pkg/api/meta"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/mrajashree/backup/pkg/apis/backupper.cattle.io/v1"
	util "github.com/mrajashree/backup/pkg/controllers"
	backupControllers "github.com/mrajashree/backup/pkg/generated/controllers/backupper.cattle.io/v1"
	lasso "github.com/rancher/lasso/pkg/client"
	"github.com/sirupsen/logrus"

	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	//"k8s.io/apimachinery/pkg/api/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sv1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/storage/value"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

const (
	metadataMapKey  = "metadata"
	ownerRefsMapKey = "ownerReferences"
)

type handler struct {
	ctx                     context.Context
	restores                backupControllers.RestoreController
	backups                 backupControllers.BackupController
	backupEncryptionConfigs backupControllers.BackupEncryptionConfigController
	discoveryClient         discovery.DiscoveryInterface
	dynamicClient           dynamic.Interface
	sharedClientFactory     lasso.SharedClientFactory
	restmapper              meta.RESTMapper
}

type restoreObj struct {
	Name               string
	Namespace          string
	GVR                schema.GroupVersionResource
	ResourceConfigPath string
	Data               *unstructured.Unstructured
}

func Register(
	ctx context.Context,
	restores backupControllers.RestoreController,
	backups backupControllers.BackupController,
	backupEncryptionConfigs backupControllers.BackupEncryptionConfigController,
	clientSet *clientset.Clientset,
	dynamicInterface dynamic.Interface,
	sharedClientFactory lasso.SharedClientFactory,
	restmapper meta.RESTMapper) {

	controller := &handler{
		ctx:                     ctx,
		restores:                restores,
		backups:                 backups,
		backupEncryptionConfigs: backupEncryptionConfigs,
		dynamicClient:           dynamicInterface,
		discoveryClient:         clientSet.Discovery(),
		sharedClientFactory:     sharedClientFactory,
		restmapper:              restmapper,
	}

	// Register handlers
	restores.OnChange(ctx, "restore", controller.OnRestoreChange)
}

func (h *handler) OnRestoreChange(_ string, restore *v1.Restore) (*v1.Restore, error) {
	created := make(map[string]bool)
	ownerToDependentsList := make(map[string][]restoreObj)
	var toRestore []restoreObj
	numOwnerReferences := make(map[string]int)
	resourcesWithStatusSubresource := make(map[string]bool)

	backupName := restore.Spec.BackupFilename

	backupPath, err := ioutil.TempDir("", strings.TrimSuffix(backupName, ".tar.gz"))
	if err != nil {
		return restore, err
	}
	logrus.Infof("Temporary path for un-tar/gzip backup data during restore: %v", backupPath)

	backupLocation := restore.Spec.StorageLocation
	if backupLocation == nil {
		return restore, fmt.Errorf("Specify backup location during restore")
	}
	if backupLocation.Local != "" {
		// if local, backup tar.gz must be added to the "Local" path
		backupFilePath := filepath.Join(backupLocation.Local, backupName)
		if err := util.LoadFromTarGzip(backupFilePath, backupPath); err != nil {
			removeDirErr := os.RemoveAll(backupPath)
			if removeDirErr != nil {
				return restore, errors.New(err.Error() + removeDirErr.Error())
			}
			return restore, err
		}
	} else if backupLocation.S3 != nil {
		backupFilePath, err := h.downloadFromS3(restore)
		if err != nil {
			removeDirErr := os.RemoveAll(backupPath)
			if removeDirErr != nil {
				return restore, errors.New(err.Error() + removeDirErr.Error())
			}
			removeFileErr := os.Remove(backupFilePath)
			if removeFileErr != nil {
				return restore, errors.New(err.Error() + removeFileErr.Error())
			}
			return restore, err
		}
		if err := util.LoadFromTarGzip(backupFilePath, backupPath); err != nil {
			removeDirErr := os.RemoveAll(backupPath)
			if removeDirErr != nil {
				return restore, errors.New(err.Error() + removeDirErr.Error())
			}
			removeFileErr := os.Remove(backupFilePath)
			if removeFileErr != nil {
				return restore, errors.New(err.Error() + removeFileErr.Error())
			}
			return restore, err
		}
		// remove the downloaded gzip file from s3 as contents are untar/unzipped at the temp location by this point
		removeFileErr := os.Remove(backupFilePath)
		if removeFileErr != nil {
			return restore, errors.New(err.Error() + removeFileErr.Error())
		}
	}
	backupPath = strings.TrimSuffix(backupPath, ".tar.gz")
	logrus.Infof("Untar/Ungzip backup at %v", backupPath)
	config, err := h.backupEncryptionConfigs.Get("default", restore.Spec.EncryptionConfigName, k8sv1.GetOptions{})
	if err != nil {
		removeDirErr := os.RemoveAll(backupPath)
		if removeDirErr != nil {
			return restore, errors.New(err.Error() + removeDirErr.Error())
		}
		return restore, err
	}
	transformerMap, err := util.GetEncryptionTransformers(config)
	if err != nil {
		removeDirErr := os.RemoveAll(backupPath)
		if removeDirErr != nil {
			return restore, errors.New(err.Error() + removeDirErr.Error())
		}
		return restore, err
	}

	// first restore CRDs
	startTime := time.Now()
	fmt.Printf("\nStart time: %v\n", startTime)
	if err := h.restoreCRDs(backupPath, transformerMap, created); err != nil {
		logrus.Errorf("\nerror during restoreCRDs: %v\n", err)
		removeDirErr := os.RemoveAll(backupPath)
		if removeDirErr != nil {
			return restore, errors.New(err.Error() + removeDirErr.Error())
		}
		panic(err)
		return restore, err
	}
	timeForRestoringCRDs := time.Since(startTime)
	fmt.Printf("\ntime taken to restore CRDs: %v\n", timeForRestoringCRDs)
	doneRestoringCRDTime := time.Now()

	if err := h.findResourcesWithStatusSubresource(backupPath, resourcesWithStatusSubresource); err != nil {
		removeDirErr := os.RemoveAll(backupPath)
		if removeDirErr != nil {
			return restore, errors.New(err.Error() + removeDirErr.Error())
		}
		return restore, err
	}
	fmt.Printf("\nsubresource graph: %v\n", resourcesWithStatusSubresource)

	// generate adjacency lists for dependents and ownerRefs
	if err := h.generateDependencyGraph(backupPath, transformerMap, ownerToDependentsList, &toRestore, numOwnerReferences); err != nil {
		logrus.Errorf("\nerror during generateDependencyGraph: %v\n", err)
		removeDirErr := os.RemoveAll(backupPath)
		if removeDirErr != nil {
			return restore, errors.New(err.Error() + removeDirErr.Error())
		}
		panic(err)
		return restore, err
	}
	timeForGeneratingGraph := time.Since(doneRestoringCRDTime)
	fmt.Printf("\ntime taken to generate graph: %v\n", timeForGeneratingGraph)

	doneGeneratingGraphTime := time.Now()
	logrus.Infof("No-goroutines-2 time right before starting to create from graph: %v", doneGeneratingGraphTime)
	if err := h.createFromDependencyGraph(ownerToDependentsList, created, numOwnerReferences, toRestore, resourcesWithStatusSubresource); err != nil {
		logrus.Errorf("\nerror during createFromDependencyGraph: %v\n", err)
		removeDirErr := os.RemoveAll(backupPath)
		if removeDirErr != nil {
			return restore, errors.New(err.Error() + removeDirErr.Error())
		}
		panic(err)
		return restore, err
	}
	timeForRestoringResources := time.Since(doneGeneratingGraphTime)
	fmt.Printf("\ntime taken to restore resources: %v\n", timeForRestoringResources)

	if restore.Spec.Prune {
		if err := h.prune(strings.TrimSuffix(backupName, ".tar.gz"), backupPath, restore.Spec.DeleteTimeout, transformerMap); err != nil {
			return restore, fmt.Errorf("error pruning during restore: %v", err)
		}
	}

	logrus.Infof("Done restoring")
	if err := os.RemoveAll(backupPath); err != nil {
		return restore, err
	}
	return restore, nil
}

func (h *handler) restoreCRDs(backupPath string, transformerMap map[schema.GroupResource]value.Transformer, created map[string]bool) error {
	// Both CRD apiversions have different way of indicating presence of status subresource
	for _, resourceGVK := range []string{"customresourcedefinitions.apiextensions.k8s.io#v1", "customresourcedefinitions.apiextensions.k8s.io#v1beta1"} {
		resourceDirPath := path.Join(backupPath, resourceGVK)
		if _, err := os.Stat(resourceDirPath); err != nil && os.IsNotExist(err) {
			continue
		}
		gvr := getGVR(resourceGVK)
		gr := gvr.GroupResource()
		decryptionTransformer, _ := transformerMap[gr]
		dirContents, err := ioutil.ReadDir(resourceDirPath)
		if err != nil {
			return err
		}
		for _, resFile := range dirContents {
			resConfigPath := filepath.Join(resourceDirPath, resFile.Name())
			crdContent, err := ioutil.ReadFile(resConfigPath)
			if err != nil {
				return err
			}
			crdName := strings.TrimSuffix(resFile.Name(), ".json")
			if decryptionTransformer != nil {
				var encryptedBytes []byte
				if err := json.Unmarshal(crdContent, &encryptedBytes); err != nil {
					return err
				}
				decrypted, _, err := decryptionTransformer.TransformFromStorage(encryptedBytes, value.DefaultContext(crdName))
				if err != nil {
					return err
				}
				crdContent = decrypted
			}
			var crdData map[string]interface{}
			if err := json.Unmarshal(crdContent, &crdData); err != nil {
				return err
			}
			restoreObjKey := restoreObj{
				Name:               crdName,
				ResourceConfigPath: resConfigPath,
				GVR:                gvr,
				Data:               &unstructured.Unstructured{Object: crdData},
			}
			err = h.restoreResource(restoreObjKey, gvr, false)
			if err != nil {
				return fmt.Errorf("restoreCRDs: %v", err)
			}

			created[restoreObjKey.ResourceConfigPath] = true
		}
	}
	return nil
}

func (h *handler) findResourcesWithStatusSubresource(backupPath string, resourcesWithStatusSubresource map[string]bool) error {
	fileBytes, err := ioutil.ReadFile(filepath.Join(backupPath, "filters", "statussubresource.json"))
	if err != nil {
		return err
	}
	err = json.Unmarshal(fileBytes, &resourcesWithStatusSubresource)
	return err
}

// generateDependencyGraph creates a graph "ownerToDependentsList" to track objects with ownerReferences
// any "node" in this graph is a map entry, where key = owning object, value = list of its dependents
// all objects that do not have ownerRefs are added to the "toRestore" list
// numOwnerReferences keeps track of how many owners any object has that haven't been restored yet
func (h *handler) generateDependencyGraph(backupPath string, transformerMap map[schema.GroupResource]value.Transformer,
	ownerToDependentsList map[string][]restoreObj, toRestore *[]restoreObj, numOwnerReferences map[string]int) error {
	backupEntries, err := ioutil.ReadDir(backupPath)
	if err != nil {
		return err
	}

	for _, backupEntry := range backupEntries {
		if backupEntry.Name() == "filters" {
			// filters directory contains filters.json & a file with resourceGVK of resources that have status subresource
			continue
		}

		// example catalogs.management.cattle.io#v3
		resourceGVK := backupEntry.Name()
		resourceDirPath := path.Join(backupPath, resourceGVK)
		gvr := getGVR(resourceGVK)
		gr := gvr.GroupResource()
		resourceDirEntries, err := ioutil.ReadDir(resourceDirPath)
		if err != nil {
			return err
		}

		for _, resourceDirEntry := range resourceDirEntries {
			var namespace string
			if resourceDirEntry.IsDir() {
				// resource is namespaced, and this subfolder's name is the namespace
				namespace = resourceDirEntry.Name()
				resourceNamespaceDirPath := path.Join(backupPath, resourceGVK, namespace)
				resourceFiles, err := ioutil.ReadDir(resourceNamespaceDirPath)
				if err != nil {
					return err
				}
				for _, resourceFile := range resourceFiles {
					resManifestPath := filepath.Join(resourceNamespaceDirPath, resourceFile.Name())
					resourceName := strings.TrimSuffix(resourceFile.Name(), ".json")
					additionalAuthenticatedData := fmt.Sprintf("%s#%s", namespace, resourceName)
					if err := h.addToOwnersToDependentsList(backupPath, resManifestPath, additionalAuthenticatedData, gvr, transformerMap[gr],
						ownerToDependentsList, toRestore, numOwnerReferences); err != nil {
						return err
					}
				}
				continue
			}
			resManifestPath := filepath.Join(resourceDirPath, resourceDirEntry.Name())
			additionalAuthenticatedData := strings.TrimSuffix(resourceDirEntry.Name(), ".json")
			if err := h.addToOwnersToDependentsList(backupPath, resManifestPath, additionalAuthenticatedData, gvr, transformerMap[gr],
				ownerToDependentsList, toRestore, numOwnerReferences); err != nil {
				return err
			}
		}
	}
	return nil
}

// addToOwnersToDependentsList reads given file, if there are no ownerRefs in that file, adds it to "toRestore"
/* if the file has ownerRefences:
1. it iterates over each ownerRef,
2. creates an entry for each owner in ownerToDependentsList", with the current object in the value list
3. gets total count of ownerRefs and adds current object to "numOwnerReferences" map to indicate the count*/
func (h *handler) addToOwnersToDependentsList(backupPath, resConfigPath, additionalAuthenticatedData string, gvr schema.GroupVersionResource, decryptionTransformer value.Transformer,
	ownerToDependentsList map[string][]restoreObj, toRestore *[]restoreObj, numOwnerReferences map[string]int) error {
	logrus.Infof("Processing %v for adjacency list", resConfigPath)
	resBytes, err := ioutil.ReadFile(resConfigPath)
	if err != nil {
		return err
	}

	if decryptionTransformer != nil {
		var encryptedBytes []byte
		if err := json.Unmarshal(resBytes, &encryptedBytes); err != nil {
			return err
		}
		decrypted, _, err := decryptionTransformer.TransformFromStorage(encryptedBytes, value.DefaultContext(additionalAuthenticatedData))
		if err != nil {
			return err
		}
		resBytes = decrypted
	}

	fileMap := make(map[string]interface{})
	err = json.Unmarshal(resBytes, &fileMap)
	if err != nil {
		return err
	}

	metadata, metadataFound := fileMap[metadataMapKey].(map[string]interface{})
	if !metadataFound {
		return nil
	}

	// add to adjacency list
	name, _ := metadata["name"].(string)
	namespace, isNamespaced := metadata["namespace"].(string)
	currRestoreObj := restoreObj{
		Name:               name,
		ResourceConfigPath: resConfigPath,
		GVR:                gvr,
		Data:               &unstructured.Unstructured{Object: fileMap},
	}
	if isNamespaced {
		currRestoreObj.Namespace = namespace
	}

	ownerRefs, ownerRefsFound := metadata[ownerRefsMapKey].([]interface{})
	if !ownerRefsFound {
		// has no dependents, so no need to add to adjacency list, add to restoreResources list
		*toRestore = append(*toRestore, currRestoreObj)
		return nil
	}
	numOwners := 0
	for _, owner := range ownerRefs {
		numOwners++
		ownerRefData, ok := owner.(map[string]interface{})
		if !ok {
			logrus.Errorf("invalid ownerRef")
			continue
		}

		groupVersion := ownerRefData["apiVersion"].(string)
		gv, err := schema.ParseGroupVersion(groupVersion)
		if err != nil {
			logrus.Errorf(" err %v parsing ownerRef apiVersion", err)
			continue
		}
		kind := ownerRefData["kind"].(string)
		gvk := gv.WithKind(kind)
		ownerGVR, isNamespaced, err := h.sharedClientFactory.ResourceForGVK(gvk)
		if err != nil {
			return fmt.Errorf("Error getting resource for gvk %v: %v", gvk, err)
		}

		var apiGroup, version string
		split := strings.SplitN(groupVersion, "/", 2)
		if len(split) == 1 {
			// resources under v1 version
			version = split[0]
		} else {
			apiGroup = split[0]
			version = split[1]
		}
		// TODO: check if this object creation is needed
		// kind + "." + apigroup + "#" + version
		ownerDirPath := fmt.Sprintf("%s.%s#%s", ownerGVR.Resource, apiGroup, version)
		ownerName := ownerRefData["name"].(string)
		// Store resourceConfigPath of owner Ref because that's what we check for in "Created" map
		ownerObj := restoreObj{
			Name:               ownerName,
			ResourceConfigPath: filepath.Join(backupPath, ownerDirPath, ownerName+".json"),
			GVR:                ownerGVR,
		}
		if isNamespaced {
			// if owning object is namespaced, then it has to be the same ns as the current dependent object
			ownerObj.Namespace = currRestoreObj.Namespace
			// the owner object's resourceFile in backup would also have namespace in the filename, so update
			// ownerObj.ResourceConfigPath to include namespace subdir before the filename for owner
			ownerFilename := filepath.Join(currRestoreObj.Namespace, ownerName+".json")
			ownerObj.ResourceConfigPath = filepath.Join(backupPath, ownerDirPath, ownerFilename)
		}
		ownerObjDependents, ok := ownerToDependentsList[ownerObj.ResourceConfigPath]
		if !ok {
			ownerToDependentsList[ownerObj.ResourceConfigPath] = []restoreObj{currRestoreObj}
		} else {
			ownerToDependentsList[ownerObj.ResourceConfigPath] = append(ownerObjDependents, currRestoreObj)
		}
	}

	numOwnerReferences[currRestoreObj.ResourceConfigPath] = numOwners
	return nil
}

func (h *handler) createFromDependencyGraph(ownerToDependentsList map[string][]restoreObj, created map[string]bool,
	numOwnerReferences map[string]int, toRestore []restoreObj, resourcesWithStatusSubresource map[string]bool) error {
	numTotalDependents := 0
	for _, dependents := range ownerToDependentsList {
		numTotalDependents += len(dependents)
	}
	countRestored := 0
	var errList []error
	for len(toRestore) > 0 {
		curr := toRestore[0]
		if len(toRestore) == 1 {
			toRestore = []restoreObj{}
		} else {
			toRestore = toRestore[1:]
		}
		if created[curr.ResourceConfigPath] {
			logrus.Infof("Resource %v is already created", curr.ResourceConfigPath)
			continue
		}
		// TODO add resourcename to error to print summary
		// TODO if owner not found, it has to be cross-namespaced dependency, so still create this obj: log this
		// log if you're dropping ownerRefs
		if err := h.restoreResource(curr, curr.GVR, resourcesWithStatusSubresource[curr.GVR.String()]); err != nil {
			errList = append(errList, err)
			continue
		}
		for _, dependent := range ownerToDependentsList[curr.ResourceConfigPath] {
			// example, curr = catTemplate, dependent=catTempVer
			if numOwnerReferences[dependent.ResourceConfigPath] > 0 {
				numOwnerReferences[dependent.ResourceConfigPath]--
			}
			if numOwnerReferences[dependent.ResourceConfigPath] == 0 {
				logrus.Infof("dependent %v is now ready to create", dependent.Name)
				toRestore = append(toRestore, dependent)
			}
		}
		created[curr.ResourceConfigPath] = true
		countRestored++
	}
	// TODO: LOG all skipped objects with reasons
	fmt.Printf("\nTotal restored resources final: %v\n", countRestored)
	return util.ErrList(errList)
}

func (h *handler) updateOwnerRefs(ownerReferences []interface{}, namespace string) error {
	for ind, ownerRef := range ownerReferences {
		reference := ownerRef.(map[string]interface{})
		apiversion, _ := reference["apiVersion"].(string)
		kind, _ := reference["kind"].(string)
		if apiversion == "" || kind == "" {
			continue
		}
		ownerGV, err := schema.ParseGroupVersion(apiversion)
		if err != nil {
			return fmt.Errorf("err %v parsing apiversion %v", err, apiversion)
		}
		ownerGVK := ownerGV.WithKind(kind)
		name, _ := reference["name"].(string)

		ownerGVR, isNamespaced, err := h.sharedClientFactory.ResourceForGVK(ownerGVK)
		if err != nil {
			return fmt.Errorf("error getting resource for gvk %v: %v", ownerGVK, err)
		}
		ownerObj := &restoreObj{
			Name: name,
			GVR:  ownerGVR,
		}
		// ns.OwnerRef = cluster
		// namespace can only be owned by cluster-scoped objects, SO
		// CRDS, cluster-scoped, then namespaced
		// obj in ns A has owner ref to obj in ns B: what t
		// TODO: restore cluster-scoped first then namespaced
		// ns.ownerRefs
		// if owner object is namespaced, it has to be within same namespace, since per definition
		/*
			// OwnerReference contains enough information to let you identify an owning
			// object. An owning object must be in the same namespace as the dependent, or
			// be cluster-scoped, so there is no namespace field.*/
		if isNamespaced {
			ownerObj.Namespace = namespace
		}

		logrus.Infof("Getting new UID for %v ", ownerObj.Name)
		ownerObjNewUID, err := h.getOwnerNewUID(ownerObj)
		if err != nil {
			// not found error should be handled separately
			// continue trying to get UIDs of other owners?
			// obj in ns A has owner ref to obj in ns B: check what err is, mostly not found
			return fmt.Errorf("error obtaining new UID for %v: %v", ownerObj.Name, err)
		}
		reference["uid"] = ownerObjNewUID
		ownerReferences[ind] = reference
	}
	return nil
}

func (h *handler) restoreResource(currRestoreObj restoreObj, gvr schema.GroupVersionResource, hasStatusSubresource bool) error {
	logrus.Infof("Restoring %v", currRestoreObj.Name)

	fileMap := currRestoreObj.Data.Object
	obj := currRestoreObj.Data

	fileMapMetadata := fileMap[metadataMapKey].(map[string]interface{})
	name := fileMapMetadata["name"].(string)
	namespace, _ := fileMapMetadata["namespace"].(string)
	var dr dynamic.ResourceInterface
	dr = h.dynamicClient.Resource(gvr)
	if namespace != "" {
		dr = h.dynamicClient.Resource(gvr).Namespace(namespace)
	}
	ownerReferences, _ := fileMapMetadata[ownerRefsMapKey].([]interface{})
	if ownerReferences != nil {
		// no-cross ns, restoreA: error, network
		if err := h.updateOwnerRefs(ownerReferences, namespace); err != nil {
			return err
		}
	}
	res, err := dr.Get(h.ctx, name, k8sv1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("restoreResource: err getting resource %v", err)
		}
		// create and return
		createdObj, err := dr.Create(h.ctx, obj, k8sv1.CreateOptions{})
		if err != nil {
			return err
		}
		if hasStatusSubresource {
			logrus.Infof("Updating status subresource for %#v", currRestoreObj.Name)
			_, err := dr.UpdateStatus(h.ctx, createdObj, k8sv1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("restoreResource: err updating status resource %v", err)
			}
		}
		return nil
	}
	resMetadata := res.Object[metadataMapKey].(map[string]interface{})
	resourceVersion := resMetadata["resourceVersion"].(string)
	obj.Object[metadataMapKey].(map[string]interface{})["resourceVersion"] = resourceVersion
	_, err = dr.Update(h.ctx, obj, k8sv1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("restoreResource: err updating resource %v", err)
	}
	if hasStatusSubresource {
		logrus.Infof("Updating status subresource for %#v", currRestoreObj.Name)
		_, err := dr.UpdateStatus(h.ctx, obj, k8sv1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("restoreResource: err updating status resource %v", err)
		}
	}

	fmt.Printf("\nSuccessfully restored %v\n", name)
	return nil
}

func (h *handler) getOwnerNewUID(owner *restoreObj) (string, error) {
	var ownerDyn dynamic.ResourceInterface
	ownerDyn = h.dynamicClient.Resource(owner.GVR)

	if owner.Namespace != "" {
		ownerDyn = h.dynamicClient.Resource(owner.GVR).Namespace(owner.Namespace)
	}
	ownerObj, err := ownerDyn.Get(h.ctx, owner.Name, k8sv1.GetOptions{})
	if err != nil {
		return "", err
	}
	ownerObjMetadata := ownerObj.Object[metadataMapKey].(map[string]interface{})
	ownerObjUID := ownerObjMetadata["uid"].(string)
	return ownerObjUID, nil
}

// getGVR parses the directory path to provide groupVersionResource
func getGVR(resourceGVK string) schema.GroupVersionResource {
	gvkParts := strings.Split(resourceGVK, "#")
	version := gvkParts[1]
	resourceGroup := strings.SplitN(gvkParts[0], ".", 2)
	resource := strings.TrimSuffix(resourceGroup[0], ".")
	var group string
	if len(resourceGroup) > 1 {
		group = resourceGroup[1]
	}
	gr := schema.ParseGroupResource(resource + "." + group)
	gvr := gr.WithVersion(version)
	return gvr
}
