/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	cloudresourcemanager "google.golang.org/api/cloudresourcemanager/v1"
	"k8s.io/klog"
	boskosclient "k8s.io/test-infra/boskos/client"
	"k8s.io/test-infra/boskos/common"
	utilcommon "sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/common"
	remote "sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/test/remote"
)

var (
	boskos = boskosclient.NewClient(os.Getenv("JOB_NAME"), "http://boskos")
)

func GCEClientAndDriverSetup(instance *remote.InstanceInfo) (*remote.TestContext, error) {
	port := fmt.Sprintf("%v", 1024+rand.Intn(10000))
	goPath, ok := os.LookupEnv("GOPATH")
	if !ok {
		return nil, fmt.Errorf("Could not find environment variable GOPATH")
	}
	pkgPath := path.Join(goPath, "src/sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/")
	binPath := path.Join(pkgPath, "bin/gce-pd-csi-driver")

	endpoint := fmt.Sprintf("tcp://localhost:%s", port)

	workspace := remote.NewWorkspaceDir("gce-pd-e2e-")
	driverRunCmd := fmt.Sprintf("sh -c '/usr/bin/nohup %s/gce-pd-csi-driver --endpoint=%s> %s/prog.out 2> %s/prog.err < /dev/null &'",
		workspace, endpoint, workspace, workspace)

	config := &remote.ClientConfig{
		PkgPath:      pkgPath,
		BinPath:      binPath,
		WorkspaceDir: workspace,
		RunDriverCmd: driverRunCmd,
		Port:         port,
	}

	err := os.Setenv("GCE_PD_CSI_STAGING_VERSION", "latest")
	if err != nil {
		return nil, err
	}

	return remote.SetupNewDriverAndClient(instance, config)
}

// getBoskosProject retries acquiring a boskos project until success or timeout
func getBoskosProject(resourceType string) *common.Resource {
	timer := time.NewTimer(30 * time.Minute)
	defer timer.Stop()
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-timer.C:
			klog.Fatalf("timed out trying to acquire boskos project")
		case <-ticker.C:
			p, err := boskos.Acquire(resourceType, "free", "busy")
			if err != nil {
				klog.Warningf("boskos failed to acquire project: %v", err)
			} else if p == nil {
				klog.Warningf("boskos does not have a free %s at the moment", resourceType)
			} else {
				return p
			}
		}
	}

}

func SetupProwConfig(resourceType string) (project, serviceAccount string) {
	// Try to get a Boskos project
	klog.V(4).Infof("Running in PROW")
	klog.V(4).Infof("Fetching a Boskos loaned project")

	p := getBoskosProject(resourceType)
	project = p.GetName()

	go func(c *boskosclient.Client, proj string) {
		for range time.Tick(time.Minute * 5) {
			if err := c.UpdateOne(p.Name, "busy", nil); err != nil {
				klog.Warningf("[Boskos] Update %s failed with %v", p.Name, err)
			}
		}
	}(boskos, p.Name)

	// If we're on CI overwrite the service account
	klog.V(4).Infof("Fetching the default compute service account")

	c, err := google.DefaultClient(context.Background(), cloudresourcemanager.CloudPlatformScope)
	if err != nil {
		klog.Fatalf("Failed to get Google Default Client: %v", err)
	}

	cloudresourcemanagerService, err := cloudresourcemanager.New(c)
	if err != nil {
		klog.Fatalf("Failed to create new cloudresourcemanager: %v", err)
	}

	resp, err := cloudresourcemanagerService.Projects.Get(project).Do()
	if err != nil {
		klog.Fatalf("Failed to get project %v from Cloud Resource Manager: %v", project, err)
	}

	// Default Compute Engine service account
	// [PROJECT_NUMBER]-compute@developer.gserviceaccount.com
	serviceAccount = fmt.Sprintf("%v-compute@developer.gserviceaccount.com", resp.ProjectNumber)
	klog.Infof("Using project %v and service account %v", project, serviceAccount)
	return project, serviceAccount
}

func ForceChmod(instance *remote.InstanceInfo, filePath string, perms string) error {
	originalumask, err := instance.SSHNoSudo("umask")
	if err != nil {
		return fmt.Errorf("failed to umask. Output: %v, errror: %v", originalumask, err)
	}
	output, err := instance.SSHNoSudo("umask", "0000")
	if err != nil {
		return fmt.Errorf("failed to umask. Output: %v, errror: %v", output, err)
	}
	output, err = instance.SSH("chmod", "-R", perms, filePath)
	if err != nil {
		return fmt.Errorf("failed to chmod file %s. Output: %v, errror: %v", filePath, output, err)
	}
	output, err = instance.SSHNoSudo("umask", originalumask)
	if err != nil {
		return fmt.Errorf("failed to umask. Output: %v, errror: %v", output, err)
	}
	return nil
}

func WriteFile(instance *remote.InstanceInfo, filePath, fileContents string) error {
	output, err := instance.SSHNoSudo("echo", fileContents, ">", filePath)
	if err != nil {
		return fmt.Errorf("failed to write test file %s. Output: %v, errror: %v", filePath, output, err)
	}
	return nil
}

func ReadFile(instance *remote.InstanceInfo, filePath string) (string, error) {
	output, err := instance.SSHNoSudo("cat", filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read test file %s. Output: %v, errror: %v", filePath, output, err)
	}
	return output, nil
}

func GetFSSizeInGb(instance *remote.InstanceInfo, mountPath string) (int64, error) {
	output, err := instance.SSHNoSudo("df", "--output=size", "-BG", mountPath, "|", "awk", "'NR==2'")
	if err != nil {
		return -1, fmt.Errorf("failed to get size of path %s. Output: %v, error: %v", mountPath, output, err)
	}
	output = strings.TrimSuffix(strings.TrimSpace(output), "G")
	n, err := strconv.ParseInt(output, 10, 64)
	if err != nil {
		return -1, fmt.Errorf("failed to parse size %s into int", output)
	}
	return n, nil
}

func GetBlockSizeInGb(instance *remote.InstanceInfo, devicePath string) (int64, error) {
	output, err := instance.SSH("blockdev", "--getsize64", devicePath)
	if err != nil {
		return -1, fmt.Errorf("failed to get size of path %s. Output: %v, error: %v", devicePath, output, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return -1, fmt.Errorf("failed to parse size %s into int", output)
	}
	return utilcommon.BytesToGb(n), nil
}

func RmAll(instance *remote.InstanceInfo, filePath string) error {
	output, err := instance.SSH("rm", "-rf", filePath)
	if err != nil {
		return fmt.Errorf("failed to delete all %s. Output: %v, errror: %v", filePath, output, err)
	}
	return nil
}
