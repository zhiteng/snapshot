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
	"flag"
	"os"
	"os/signal"
	"time"

	"github.com/golang/glog"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rootfs/snapshot/pkg/client"
	"github.com/rootfs/snapshot/pkg/cloudprovider"
	"github.com/rootfs/snapshot/pkg/cloudprovider/providers/aws"
	snapshotcontroller "github.com/rootfs/snapshot/pkg/controller/snapshot-controller"
	"github.com/rootfs/snapshot/pkg/volume"
	"github.com/rootfs/snapshot/pkg/volume/aws_ebs"
	"github.com/rootfs/snapshot/pkg/volume/hostpath"
)

const (
	defaultSyncDuration time.Duration = 60 * time.Second
)

var (
	kubeconfig      = flag.String("kubeconfig", "", "Path to a kube config. Only required if out-of-cluster.")
	cloudProvider   = flag.String("cloudprovider", "", "aws|gcp|openstack|azure")
	cloudConfigFile = flag.String("cloudconfig", "", "Path to a Cloud config. Only required if cloudprovider is set.")
	volumePlugins   = make(map[string]volume.VolumePlugin)
)

func main() {
	flag.Parse()
	flag.Set("logtostderr", "true")
	// Create the client config. Use kubeconfig if given, otherwise assume in-cluster.
	config, err := buildConfig(*kubeconfig)
	if err != nil {
		panic(err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	// initialize third party resource if it does not exist
	err = client.CreateTPR(clientset)
	if err != nil {
		panic(err)
	}

	// make a new config for our extension's API group, using the first config as a baseline
	snapshotClient, snapshotScheme, err := client.NewClient(config)
	if err != nil {
		panic(err)
	}

	// wait until TPR gets processed
	err = client.WaitForSnapshotResource(snapshotClient)
	if err != nil {
		panic(err)
	}
	// build volume plugins map
	buildVolumePlugins()

	// start controller on instances of our TPR
	glog.Infof("starting snapshot controller")
	ssController := snapshotcontroller.NewSnapshotController(snapshotClient, snapshotScheme, clientset, &volumePlugins, defaultSyncDuration)
	stopCh := make(chan struct{})

	go ssController.Run(stopCh)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
	close(stopCh)

}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func buildVolumePlugins() {
	if len(*cloudProvider) != 0 {
		cloud, err := cloudprovider.InitCloudProvider(*cloudProvider, *cloudConfigFile)
		if err == nil && cloud != nil {
			if *cloudProvider == aws.ProviderName {
				awsPlugin := aws_ebs.RegisterPlugin()
				awsPlugin.Init(cloud)
				volumePlugins[aws_ebs.GetPluginName()] = awsPlugin
			}
		} else {
			glog.Warningf("failed to initialize aws cloudprovider: %v, supported cloudproviders are %#v", err, cloudprovider.CloudProviders())
		}
	}

	volumePlugins[hostpath.GetPluginName()] = hostpath.RegisterPlugin()
}
