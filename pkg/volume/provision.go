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

package volume

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/v8/controller"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/v8/gidallocator"
)

const (
	// are we allowed to set this? else make up our own
	annCreatedBy       = "kubernetes.io/createdby"
	createdBy          = "glusterfs-simple-provisioner"
	dynamicEpSvcPrefix = "glusterfs-simple-"
)

// NewGlusterfsProvisioner creates a new glusterfs simple provisioner
func NewGlusterfsProvisioner(config *rest.Config, client kubernetes.Interface) controller.Provisioner {
	klog.Infof("Creating NewGlusterfsProvisioner.")
	return newGlusterfsProvisionerInternal(config, client)
}

func newGlusterfsProvisionerInternal(config *rest.Config, client kubernetes.Interface) *glusterfsProvisioner {
	var identity types.UID

	restClient := client.CoreV1().RESTClient()
	provisioner := &glusterfsProvisioner{
		config:     config,
		client:     client,
		restClient: restClient,
		identity:   identity,
		allocator:  gidallocator.New(client),
	}

	return provisioner
}

type glusterfsProvisioner struct {
	client     kubernetes.Interface
	restClient rest.Interface
	config     *rest.Config
	identity   types.UID
	allocator  gidallocator.Allocator
}

type glusterBrick struct {
	Host string
	Path string
}

var _ controller.Provisioner = &glusterfsProvisioner{}

func (p *glusterfsProvisioner) Provision(
	ctx context.Context,
	options controller.ProvisionOptions) (*v1.PersistentVolume, controller.ProvisioningState, error) {
	if options.PVC.Spec.Selector != nil {
		return nil, controller.ProvisioningFinished, fmt.Errorf("claim Selector is not supported")
	}
	klog.V(4).Infof("Start Provisioning volume: VolumeOptions %v", options)

	gid, err := p.allocator.AllocateNext(options)
	if err != nil {
		return nil, controller.ProvisioningFinished, err
	}

	pvcNamespace := options.PVC.Namespace
	pvcName := options.PVC.Name

	cfg, err := NewProvisionerConfig(options.PVName, options.StorageClass.Parameters)
	if err != nil {
		return nil, controller.ProvisioningFinished, fmt.Errorf("Parameter is invalid: %s", err)
	}

	r, err := p.createVolume(ctx, pvcNamespace, pvcName, cfg, gid)
	if err != nil {
		return nil, controller.ProvisioningFinished, err
	}

	annotations := make(map[string]string)
	annotations[annCreatedBy] = createdBy
	annotations[gidallocator.VolumeGidAnnotationKey] = strconv.FormatInt(int64(gid), 10)
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        options.PVName,
			Annotations: annotations,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *options.StorageClass.ReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): options.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)],
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				Glusterfs: r,
			},
		},
	}
	return pv, controller.ProvisioningFinished, nil
}

func (p *glusterfsProvisioner) getClusterNodes(cfg *ProvisionerConfig) []string {
	// XXX: Improve to get all cluster nodes
	nodes := make([]string, len(cfg.BrickRootPaths))
	for i, root := range cfg.BrickRootPaths {
		nodes[i] = root.Host
	}
	return nodes
}

func (p *glusterfsProvisioner) createVolume(
	ctx context.Context,
	namespace string, name string,
	cfg *ProvisionerConfig,
	gid int,
) (*v1.GlusterfsPersistentVolumeSource, error) {
	var err error
	var bricks []glusterBrick
	var endpoint *v1.Endpoints
	var service *v1.Service

	bricks, err = p.createBricks(ctx, namespace, name, cfg, gid)
	if err != nil {
		klog.Errorf("Creating bricks is failed: %s,%s", namespace, name)
	}

	if err == nil {
		err = p.createGlusterVolume(ctx, bricks, cfg)
	}

	if err == nil {
		epServiceName := dynamicEpSvcPrefix + name
		epNamespace := namespace
		dynamicHostIps := p.getClusterNodes(cfg)
		endpoint, service, err = p.createEndpointService(ctx, epNamespace, epServiceName, dynamicHostIps, name)

		if err != nil {
			klog.Errorf("glusterfs: failed to create endpoint/service: %v", err)
		} else {
			klog.V(3).Infof("glusterfs: dynamic ep %v and svc : %v ", endpoint, service)
			return &v1.GlusterfsPersistentVolumeSource{
				EndpointsName: endpoint.Name,
				Path:          cfg.VolumeName,
				ReadOnly:      false,
			}, nil
		}
	}

	p.deleteVolume(ctx, namespace, name, cfg)
	return nil, err
}

func (p *glusterfsProvisioner) createBricks(
	ctx context.Context,
	namespace string, pvcName string,
	cfg *ProvisionerConfig,
	gid int,
) ([]glusterBrick, error) {
	var cmds []string
	bricks := make([]glusterBrick, len(cfg.BrickRootPaths))
	brickName := strings.Join([]string{pvcName, cfg.VolumeName}, "-")

	for i, root := range cfg.BrickRootPaths {
		host := root.Host
		path := filepath.Join(root.Path, namespace, brickName)
		bricks[i].Host = host
		bricks[i].Path = path

		klog.Infof("mkdir -p %s:%s", host, path)
		cmds = []string{
			fmt.Sprintf("mkdir -p %s", path),
			fmt.Sprintf("chown :%v %s", gid, path),
			fmt.Sprintf("chmod 0771 %s", path),
		}
		err := p.ExecuteCommands(ctx, host, cmds, cfg)
		if err != nil {
			return nil, err
		}
	}

	return bricks, nil
}

func (p *glusterfsProvisioner) createGlusterVolume(
	ctx context.Context,
	bricks []glusterBrick,
	cfg *ProvisionerConfig,
) error {
	cmd := fmt.Sprintf(
		"gluster --mode=script volume create %s %s", cfg.VolumeName, cfg.VolumeType,
	)
	for _, b := range bricks {
		cmd += fmt.Sprintf(" %s:%s", b.Host, b.Path)
	}
	if cfg.ForceCreate {
		cmd += " force"
	}

	cmds := []string{
		cmd,
		fmt.Sprintf("gluster --mode=script volume start %s", cfg.VolumeName),
	}
	// XXX: Fix this simple host determination
	host := bricks[0].Host

	// Create and Start gluster volume
	err := p.ExecuteCommands(ctx, host, cmds, cfg)
	if err != nil {
		klog.Errorf("Failed to create gluster volume: %v", cmds)
		return err
	}
	return nil
}

func (p *glusterfsProvisioner) createEndpointService(
	ctx context.Context,
	namespace string, epServiceName string,
	hostips []string,
	pvcname string,
) (endpoint *v1.Endpoints, service *v1.Service, err error) {

	addrlist := make([]v1.EndpointAddress, len(hostips))
	for i, v := range hostips {
		addrlist[i].IP = v
	}
	endpoint = &v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      epServiceName,
			Labels: map[string]string{
				"gluster.kubernetes.io/provisioned-for-pvc": pvcname,
			},
		},
		Subsets: []v1.EndpointSubset{{
			Addresses: addrlist,
			Ports:     []v1.EndpointPort{{Port: 1, Protocol: "TCP"}},
		}},
	}
	kubeClient := p.client
	if kubeClient == nil {
		return nil, nil, fmt.Errorf("glusterfs: failed to get kube client when creating endpoint service")
	}
	_, err = kubeClient.CoreV1().Endpoints(namespace).Create(ctx, endpoint, metav1.CreateOptions{})
	if err != nil && errors.IsAlreadyExists(err) {
		klog.V(1).Infof("glusterfs: endpoint [%s] already exist in namespace [%s]", endpoint, namespace)
		err = nil
	}
	if err != nil {
		klog.Errorf("glusterfs: failed to create endpoint: %v", err)
		return nil, nil, fmt.Errorf("error creating endpoint: %v", err)
	}
	service = &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      epServiceName,
			Namespace: namespace,
			Labels: map[string]string{
				"gluster.kubernetes.io/provisioned-for-pvc": pvcname,
			},
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{Protocol: "TCP", Port: 1}}}}
	_, err = kubeClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil && errors.IsAlreadyExists(err) {
		klog.V(1).Infof("glusterfs: service [%s] already exist in namespace [%s]", service, namespace)
		err = nil
	}
	if err != nil {
		klog.Errorf("glusterfs: failed to create service: %v", err)
		return nil, nil, fmt.Errorf("error creating service: %v", err)
	}
	return endpoint, service, nil
}
