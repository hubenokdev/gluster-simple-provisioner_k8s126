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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"path/filepath"
	"strings"

	"k8s.io/api/core/v1"
	"k8s.io/klog"
)

func (p *glusterfsProvisioner) Delete(ctx context.Context, volume *v1.PersistentVolume) error {
	var err error
	class, err := GetClassForVolume(ctx, p.client, volume)
	if err != nil {
		klog.Errorf("Fail to get class for volume: %v", volume)
		return err
	}
	cfg, err := NewProvisionerConfig(volume.Name, class.Parameters)
	if err != nil {
		return fmt.Errorf("Parameter is invalid: %s", err)
	}

	pvc := volume.Spec.ClaimRef
	if pvc == nil {
		klog.Errorf("glusterfs: ClaimRef is nil")
		return fmt.Errorf("glusterfs: ClaimRef is nil")
	}
	if pvc.Namespace == "" {
		klog.Errorf("glusterfs: namespace is nil")
		return fmt.Errorf("glusterfs: namespace is nil")
	}
	p.deleteVolume(ctx, pvc.Namespace, pvc.Name, cfg)

	//TODO ignorederror
	err = p.allocator.Release(volume)
	if err != nil {
		klog.Errorf("glusterfs: error to release GID: %v", err)
	}

	return nil
}

func (p *glusterfsProvisioner) deleteVolume(
	ctx context.Context,
	namespace string, name string,
	cfg *ProvisionerConfig,
) {

	p.deleteGlusterVolume(ctx, namespace, name, cfg)
	p.deleteBricks(ctx, namespace, name, cfg)

	epServiceName := dynamicEpSvcPrefix + name
	err := p.deleteEndpointService(ctx, namespace, epServiceName)
	if err != nil {
		klog.Errorf("glusterfs: error deleting endpoint %s/%s: %v", namespace, epServiceName, err)
	}

	return
}

func (p *glusterfsProvisioner) deleteGlusterVolume(
	ctx context.Context,
	namespace string, name string,
	cfg *ProvisionerConfig,
) {
	var cmds []string
	var err error
	host := cfg.BrickRootPaths[0].Host

	cmds = []string{
		fmt.Sprintf("gluster --mode=script volume stop %s force", cfg.VolumeName),
	}

	err = p.ExecuteCommands(ctx, host, cmds, cfg)
	if err != nil {
		klog.Errorf("glusterfs: failed to stop volume: %s", cfg.VolumeName)
	} else {
		cmds = []string{fmt.Sprintf(
			"gluster --mode=script volume delete %s", cfg.VolumeName,
		)}
		err = p.ExecuteCommands(ctx, host, cmds, cfg)
		if err != nil {
			klog.Errorf("glusterfs: failed to delete volume: %s", cfg.VolumeName)
		}
	}

	return
}

func (p *glusterfsProvisioner) deleteBricks(ctx context.Context,
	namespace string, pvcName string,
	cfg *ProvisionerConfig,
) {
	var cmds []string
	brickName := strings.Join([]string{pvcName, cfg.VolumeName}, "-")

	for _, root := range cfg.BrickRootPaths {
		host := root.Host
		path := filepath.Join(root.Path, namespace, brickName)

		klog.Infof("rm -rf %s:%s", host, path)
		cmds = []string{
			fmt.Sprintf("rm -rf %s", path),
		}
		err := p.ExecuteCommands(ctx, host, cmds, cfg)
		if err != nil {
			klog.Errorf("Failed to delete brick: %s: %s, %v", host, path, err)
		}
	}
}

func (p *glusterfsProvisioner) deleteEndpointService(ctx context.Context, namespace string, epServiceName string) (err error) {
	kubeClient := p.client
	if kubeClient == nil {
		return fmt.Errorf("glusterfs: failed to get kube client when deleting endpoint service")
	}
	err = kubeClient.CoreV1().Services(namespace).Delete(ctx, epServiceName, metav1.DeleteOptions{})
	if err != nil {
		klog.Errorf("glusterfs: error deleting service %s/%s: %v", namespace, epServiceName, err)
	}
	if err == nil {
		klog.V(1).Infof("glusterfs: service/endpoint %s/%s deleted successfully", namespace, epServiceName)
	}
	return nil
}
