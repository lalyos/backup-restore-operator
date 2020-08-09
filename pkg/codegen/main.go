package main

import (
	"github.com/mrajashree/backup/pkg/crds"
	"os"

	v1 "github.com/mrajashree/backup/pkg/apis/resources.cattle.io/v1"
	controllergen "github.com/rancher/wrangler/pkg/controller-gen"
	"github.com/rancher/wrangler/pkg/controller-gen/args"
)

func main() {
	os.Unsetenv("GOPATH")
	controllergen.Run(args.Options{
		OutputPackage: "github.com/mrajashree/backup/pkg/generated",
		Boilerplate:   "scripts/boilerplate.go.txt",
		Groups: map[string]args.Group{
			"resources.cattle.io": {
				Types: []interface{}{
					v1.Backup{},
					v1.ResourceSet{},
					v1.Restore{},
				},
				GenerateTypes: true,
			},
		},
	})
	err := crds.WriteCRD()
	if err != nil {
		panic(err)
	}
}
