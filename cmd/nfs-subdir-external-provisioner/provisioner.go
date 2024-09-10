/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"

	storage "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	storagehelpers "k8s.io/component-helpers/storage/volume"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/v6/controller"
)

const (
	provisionerNameKey = "PROVISIONER_NAME"
	mountPath          = "/persistentvolumes"
)

var (
	provisionAttempts = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nfs_provision_attempts_total",
			Help: "The total number of provision attempts by the NFS provisioner.",
		},
		[]string{"result"},
	)
	deleteAttempts = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nfs_delete_attempts_total",
			Help: "The total number of delete attempts by the NFS provisioner.",
		},
		[]string{"result"},
	)
)

func init() {
	prometheus.MustRegister(provisionAttempts)
	prometheus.MustRegister(deleteAttempts)
}

type nfsProvisioner struct {
	client kubernetes.Interface
	server string
	path   string
}

type pvcMetadata struct {
	data        map[string]string
	labels      map[string]string
	annotations map[string]string
}

var pattern = regexp.MustCompile(`\${\.PVC\.((labels|annotations)\.(.*?)|.*?)}`)

func (meta *pvcMetadata) stringParser(str string) string {
	result := pattern.FindAllStringSubmatch(str, -1)
	for _, r := range result {
		switch r[2] {
		case "labels":
			str = strings.ReplaceAll(str, r[0], meta.labels[r[3]])
		case "annotations":
			str = strings.ReplaceAll(str, r[0], meta.annotations[r[3]])
		default:
			str = strings.ReplaceAll(str, r[0], meta.data[r[1]])
		}
	}

	return str
}

var _ controller.Provisioner = &nfsProvisioner{}

func (p *nfsProvisioner) Provision(ctx context.Context, options controller.ProvisionOptions) (*v1.PersistentVolume, controller.ProvisioningState, error) {
	if options.PVC.Spec.Selector != nil {
		return nil, controller.ProvisioningFinished, fmt.Errorf("claim Selector is not supported")
	}
	glog.V(4).Infof("nfs provisioner: VolumeOptions %v", options)

	pvcNamespace := options.PVC.Namespace
	pvcName := options.PVC.Name

	pvName := strings.Join([]string{pvcNamespace, pvcName, options.PVName}, "-")

	metadata := &pvcMetadata{
		data: map[string]string{
			"name":      pvcName,
			"namespace": pvcNamespace,
		},
		labels:      options.PVC.Labels,
		annotations: options.PVC.Annotations,
	}

	fullPath := filepath.Join(mountPath, pvName)
	path := filepath.Join(p.path, pvName)

	pathPattern, exists := options.StorageClass.Parameters["pathPattern"]
	if exists {
		customPath := metadata.stringParser(pathPattern)
		if customPath != "" {
			path = filepath.Join(p.path, customPath)
			fullPath = filepath.Join(mountPath, customPath)
		}
	}

	glog.V(4).Infof("creating path %s", fullPath)
	err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return err != nil
	}, func() error {
		return os.MkdirAll(fullPath, 0o777)
	})
	if err != nil {
		provisionAttempts.WithLabelValues("failure").Inc()
		return nil, controller.ProvisioningFinished, fmt.Errorf("unable to create directory to provision new pv: %v", err)
	}

	err = os.Chmod(fullPath, 0o777)
	if err != nil {
		provisionAttempts.WithLabelValues("failure").Inc()
		return nil, controller.ProvisioningFinished, fmt.Errorf("unable to set permissions on new directory: %v", err)
	}

	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: options.PVName,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *options.StorageClass.ReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			MountOptions:                  options.StorageClass.MountOptions,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): options.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)],
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				NFS: &v1.NFSVolumeSource{
					Server:   p.server,
					Path:     path,
					ReadOnly: false,
				},
			},
		},
	}

	// Add custom mount options if specified in StorageClass
	if mountOptions, ok := options.StorageClass.Parameters["mountOptions"]; ok {
		pv.Spec.MountOptions = append(pv.Spec.MountOptions, strings.Split(mountOptions, ",")...)
	}

	provisionAttempts.WithLabelValues("success").Inc()
	return pv, controller.ProvisioningFinished, nil
}

func (p *nfsProvisioner) Delete(ctx context.Context, volume *v1.PersistentVolume) error {
	path := volume.Spec.PersistentVolumeSource.NFS.Path
	basePath := filepath.Base(path)
	oldPath := strings.Replace(path, p.path, mountPath, 1)

	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		glog.Warningf("path %s does not exist, deletion skipped", oldPath)
		return nil
	}

	storageClass, err := p.getClassForVolume(ctx, volume)
	if err != nil {
		deleteAttempts.WithLabelValues("failure").Inc()
		return fmt.Errorf("unable to get storage class: %v", err)
	}

	onDelete := storageClass.Parameters["onDelete"]
	switch onDelete {
	case "delete":
		err := retry.OnError(retry.DefaultRetry, func(err error) bool {
			return err != nil
		}, func() error {
			return os.RemoveAll(oldPath)
		})
		if err != nil {
			deleteAttempts.WithLabelValues("failure").Inc()
			return fmt.Errorf("unable to delete directory: %v", err)
		}
		deleteAttempts.WithLabelValues("success").Inc()
		return nil
	case "retain":
		deleteAttempts.WithLabelValues("success").Inc()
		return nil
	}

	archiveOnDelete, exists := storageClass.Parameters["archiveOnDelete"]
	if exists {
		archiveBool, err := strconv.ParseBool(archiveOnDelete)
		if err != nil {
			deleteAttempts.WithLabelValues("failure").Inc()
			return fmt.Errorf("invalid archiveOnDelete value: %v", err)
		}
		if !archiveBool {
			err := retry.OnError(retry.DefaultRetry, func(err error) bool {
				return err != nil
			}, func() error {
				return os.RemoveAll(oldPath)
			})
			if err != nil {
				deleteAttempts.WithLabelValues("failure").Inc()
				return fmt.Errorf("unable to delete directory: %v", err)
			}
			deleteAttempts.WithLabelValues("success").Inc()
			return nil
		}
	}

	archivePath := filepath.Join(mountPath, "archived-"+basePath)
	glog.V(4).Infof("archiving path %s to %s", oldPath, archivePath)
	err = retry.OnError(retry.DefaultRetry, func(err error) bool {
		return err != nil
	}, func() error {
		return os.Rename(oldPath, archivePath)
	})
	if err != nil {
		deleteAttempts.WithLabelValues("failure").Inc()
		return fmt.Errorf("unable to archive directory: %v", err)
	}
	deleteAttempts.WithLabelValues("success").Inc()
	return nil
}

// getClassForVolume returns StorageClass.
func (p *nfsProvisioner) getClassForVolume(ctx context.Context, pv *v1.PersistentVolume) (*storage.StorageClass, error) {
	if p.client == nil {
		return nil, fmt.Errorf("cannot get kube client")
	}
	className := storagehelpers.GetPersistentVolumeClass(pv)
	if className == "" {
		return nil, fmt.Errorf("volume has no storage class")
	}
	class, err := p.client.StorageV1().StorageClasses().Get(ctx, className, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return class, nil
}

func (p *nfsProvisioner) checkNFSServerHealth() error {
	_, err := os.Stat(mountPath)
	return err
}

func main() {
	flag.Parse()
	flag.Set("logtostderr", "true")

	server := os.Getenv("NFS_SERVER")
	if server == "" {
		glog.Fatal("NFS_SERVER not set")
	}
	path := os.Getenv("NFS_PATH")
	if path == "" {
		glog.Fatal("NFS_PATH not set")
	}
	provisionerName := os.Getenv(provisionerNameKey)
	if provisionerName == "" {
		glog.Fatalf("environment variable %s is not set! Please set it.", provisionerNameKey)
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	var config *rest.Config
	if kubeconfig != "" {
		// Create an OutOfClusterConfig and use it to create a client for the controller
		// to use to communicate with Kubernetes
		var err error
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			glog.Fatalf("Failed to create kubeconfig: %v", err)
		}
	} else {
		// Create an InClusterConfig and use it to create a client for the controller
		// to use to communicate with Kubernetes
		var err error
		config, err = rest.InClusterConfig()
		if err != nil {
			glog.Fatalf("Failed to create config: %v", err)
		}
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Failed to create client: %v", err)
	}

	// The controller needs to know what the server version is because out-of-tree
	// provisioners aren't officially supported until 1.5
	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		glog.Fatalf("Error getting server version: %v", err)
	}

	leaderElection := true
	leaderElectionEnv := os.Getenv("ENABLE_LEADER_ELECTION")
	if leaderElectionEnv != "" {
		leaderElection, err = strconv.ParseBool(leaderElectionEnv)
		if err != nil {
			glog.Fatalf("Unable to parse ENABLE_LEADER_ELECTION env var: %v", err)
		}
	}

	clientNFSProvisioner := &nfsProvisioner{
		client: clientset,
		server: server,
		path:   path,
	}

	// Start health check goroutine
	go func() {
		for {
			if err := clientNFSProvisioner.checkNFSServerHealth(); err != nil {
				glog.Errorf("NFS server health check failed: %v", err)
			}
			time.Sleep(5 * time.Minute)
		}
	}()

	// Start metrics server
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		glog.Fatal(http.ListenAndServe(":8080", nil))
	}()

	// Start the provision controller which will dynamically provision efs NFS
	// PVs
	pc := controller.NewProvisionController(clientset,
		provisionerName,
		clientNFSProvisioner,
		serverVersion.GitVersion,
		controller.LeaderElection(leaderElection),
		controller.Threadiness(5),
		controller.FailedProvisionThreshold(10),
		controller.FailedDeleteThreshold(10),
		controller.RateLimiter(controller.DefaultControllerRateLimiter()),
	)
	// Never stops.
	pc.Run(context.Background())
}
