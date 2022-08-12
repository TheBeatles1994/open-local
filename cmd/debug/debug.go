/*
Copyright © 2021 Alibaba Group Holding Ltd.

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

package debug

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"strings"

	localv1alpha1 "github.com/alibaba/open-local/pkg/apis/storage/v1alpha1"
	clientset "github.com/alibaba/open-local/pkg/generated/clientset/versioned"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

var (
	opt = debugOption{}
)

var Cmd = &cobra.Command{
	Use:   "debug",
	Short: "hollow node",
	Run: func(cmd *cobra.Command, args []string) {
		err := Start(&opt)
		if err != nil {
			log.Fatalf("error :%s, quitting now\n", err.Error())
		}
	},
}

func init() {
	opt.addFlags(Cmd.Flags())
}

// Start will start agent
func Start(opt *debugOption) error {
	nls := &localv1alpha1.NodeLocalStorage{}
	configFile, err := ioutil.ReadFile(opt.JsonFile)
	if err != nil {
		return err
	}
	configJSON, err := yaml.YAMLToJSON(configFile)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(configJSON, nls); err != nil {
		log.Fatalf("failed to unmarshal config json to object: %v", err)
	}

	cfg, err := clientcmd.BuildConfigFromFlags(opt.Master, opt.Kubeconfig)
	if err != nil {
		return fmt.Errorf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("Error building kubernetes clientset: %s", err.Error())
	}

	localClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("Error building example clientset: %s", err.Error())
	}

	hollowNodeNames := []string{}
	nodeList, err := kubeClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, node := range nodeList.Items {
		if strings.HasPrefix(node.Name, "hollow-node") {
			hollowNodeNames = append(hollowNodeNames, node.Name)
		}
	}

	for i, nodename := range hollowNodeNames {
		log.Info("node %d/%d: %s", i, len(hollowNodeNames), nodename)
		hollow_nls, err := localClient.CsiV1alpha1().NodeLocalStorages().Get(context.Background(), nodename, metav1.GetOptions{})
		if err != nil {
			return err
		}
		hollow_nls.Status = nls.Status
		nlsCopy := hollow_nls.DeepCopy()
		_, err = localClient.CsiV1alpha1().NodeLocalStorages().UpdateStatus(context.Background(), nlsCopy, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}
