/*
Copyright 2014 The Kubernetes Authors.

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

package queuejobdispatch

import (
	"encoding/json"
	"github.com/golang/glog"
	arbv1 "github.com/IBM/multi-cluster-app-dispatcher/pkg/apis/controller/v1alpha1"
	clientset "github.com/IBM/multi-cluster-app-dispatcher/pkg/client/clientset/controller-versioned"
	clusterstateapi "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/clusterstate/api"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"math"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	arbinformers "github.com/IBM/multi-cluster-app-dispatcher/pkg/client/informers/controller-externalversion"
	informersv1 "github.com/IBM/multi-cluster-app-dispatcher/pkg/client/informers/controller-externalversion/v1"
	listersv1 "github.com/IBM/multi-cluster-app-dispatcher/pkg/client/listers/controller/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/IBM/multi-cluster-app-dispatcher/pkg/client/clientset/controller-versioned/clients"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
)

type XQueueJobAgent struct{
		AgentId         	string
		DeploymentName  	string
		queuejobclients 	*clientset.Clientset
		k8sClients      	*kubernetes.Clientset // for the update of aggr resouces
		AggrResources   	*clusterstateapi.Resource

		queueJobInformer	informersv1.XQueueJobInformer
		queueJobLister		listersv1.XQueueJobLister
		queueJobSynced		func() bool

		agentEventQueue		*cache.FIFO
}

func NewXQueueJobAgent(config string, agentEventQueue *cache.FIFO) *XQueueJobAgent {
	configStrings:=strings.Split(config, ":")
	if len(configStrings)<2 {
		return nil
	}
	glog.V(2).Infof("[Dispatcher: Agent] Creation: %s\n", "/root/kubernetes/" + configStrings[0])

	agent_config, err:=clientcmd.BuildConfigFromFlags("", "/root/kubernetes/" + configStrings[0])
	// agent_config, err:=clientcmd.BuildConfigFromFlags("", "/root/agent101config")
	if err!=nil {
		glog.V(2).Infof("[Dispatcher: Agent] Cannot crate client\n")
		return nil
	}
	qa := &XQueueJobAgent{
		AgentId:         "/root/kubernetes/" + configStrings[0],
		DeploymentName:  configStrings[1],
		queuejobclients: clientset.NewForConfigOrDie(agent_config),
		k8sClients:      kubernetes.NewForConfigOrDie(agent_config),
		AggrResources:   clusterstateapi.EmptyResource(),
	}
	qa.agentEventQueue=agentEventQueue

	if qa.queuejobclients==nil {
		glog.V(2).Infof("[Dispatcher: Agent] Cannot Create Client\n")
	} else {
		glog.V(2).Infof("[Dispatcher: Agent] %s: Create Clients Suceessfully\n", qa.AgentId)
	}

	queueJobClientForInformer, _, err := clients.NewClient(agent_config)
	if err != nil {
		panic(err)
	}

	qa.queueJobInformer = arbinformers.NewFilteredSharedInformerFactory(queueJobClientForInformer, 0,
		func(opt *metav1.ListOptions) {
		opt.LabelSelector = "IsDispatched=true"
 		},
		).XQueueJob().XQueueJobs()
	qa.queueJobInformer.Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *arbv1.XQueueJob:
					glog.V(4).Infof("Filter XQueueJob name(%s) namespace(%s)\n", t.Name, t.Namespace)
					return true
				default:
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    qa.addQueueJob,
				UpdateFunc: qa.updateQueueJob,
				DeleteFunc: qa.deleteQueueJob,
			},
		})

	qa.queueJobLister = qa.queueJobInformer.Lister()

	qa.queueJobSynced = qa.queueJobInformer.Informer().HasSynced

	qa.UpdateAggrResources()

	return qa
}


func (cc *XQueueJobAgent) addQueueJob(obj interface{}) {
	qj, ok := obj.(*arbv1.XQueueJob)
	if !ok {
		glog.Errorf("obj is not XQueueJob")
		return
	}
	cc.agentEventQueue.Add(qj)
}

func (cc *XQueueJobAgent) updateQueueJob(oldObj, newObj interface{}) {
	newQJ, ok := newObj.(*arbv1.XQueueJob)
	if !ok {
		glog.Errorf("newObj is not XQueueJob")
		return
	}
	cc.agentEventQueue.Add(newQJ)
}

func (cc *XQueueJobAgent) deleteQueueJob(obj interface{}) {
	qj, ok := obj.(*arbv1.XQueueJob)
	if !ok {
		glog.Errorf("obj is not XQueueJob")
		return
	}
	cc.agentEventQueue.Add(qj)
}



func (qa *XQueueJobAgent) Run(stopCh chan struct{}) {
	go qa.queueJobInformer.Informer().Run(stopCh)
	cache.WaitForCacheSync(stopCh, qa.queueJobSynced)
	// go wait.Until(qa.UpdateAgent, 2*time.Second, stopCh)
}

func (qa *XQueueJobAgent) DeleteXQueueJob(cqj *arbv1.XQueueJob) {
	qj_temp:=cqj.DeepCopy()
	glog.V(2).Infof("[Dispatcher: Agent] Request deletion of XQJ %s to Agent %s\n", qj_temp.Name, qa.AgentId)
	qa.queuejobclients.ArbV1().XQueueJobs(qj_temp.Namespace).Delete(qj_temp.Name,  &metav1.DeleteOptions{})
	return
}

func (qa *XQueueJobAgent) CreateXQueueJob(cqj *arbv1.XQueueJob) {
	qj_temp:=cqj.DeepCopy()
	agent_qj:=&arbv1.XQueueJob{
		TypeMeta: qj_temp.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{Name: qj_temp.Name, Namespace: qj_temp.Namespace,},
		Spec: qj_temp.Spec,
	}
	agent_qj.Status.CanRun=qj_temp.Status.CanRun
	agent_qj.Status.IsDispatched=qj_temp.Status.IsDispatched

	if agent_qj.Labels == nil {
		agent_qj.Labels = map[string]string{}
	}
	for k, v := range qj_temp.Labels {
		agent_qj.Labels[k] = v
	}
	agent_qj.Labels["IsDispatched"] = "true"

	// glog.Infof("[Agent] XQJ resourceVersion cleaned--Name:%s, Kind:%s\n", agent_qj.Name, agent_qj.Kind)
	glog.V(2).Infof("[Dispatcher: Agent] Create XQJ: %s (Status: %+v) in Agent %s\n", agent_qj.Name, agent_qj.Status, qa.AgentId)
	qa.queuejobclients.ArbV1().XQueueJobs(agent_qj.Namespace).Create(agent_qj)
	// pods, err := qa.deploymentclients.CoreV1().Pods("").List(metav1.ListOptions{})
	// if err != nil {
	// 	glog.Infof("[Agent] Cannot Access Agent================\n")
	// }
	// glog.Infof("There are %d pods in the cluster\n", len(pods.Items))
	// // for _, pod := range pods.Items {
	// 	glog.Infof("[Agent] Pod Name=%s\n",pod.Name)
	// }

	return
}

type ClusterMetricsList struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Metadata   struct {
		SelfLink string `json:"selfLink"`
	} `json:"metadata"`
	Items [] struct {
		MetricName	string    `json:"metricName"`
		MetricLabels map[string]string  `json:"metriclabels"`
		Timestamp  string `json:"timestamp"`
		Value string `json:"value"`
	} `json:"items"`
}

func (qa *XQueueJobAgent) UpdateAggrResources() error {
    glog.V(6).Infof("[Dispatcher: Agent] Getting aggregated resources for Agent ID: %s with Agent QueueJob Name: %s\n", qa.AgentId, qa.DeploymentName)

    // Read the Agent XQJ Deployment object
    if(qa.k8sClients ==nil) {
    	return nil

	}

	data, err := qa.k8sClients.RESTClient().Get().AbsPath("apis/external.metrics.k8s.io/v1beta1/namespaces/default/cluster-external-metric").DoRaw()

    if err != nil {
		glog.V(2).Infof("Failed to get metrics from deployment Agent ID: %s with Agent QueueJob Name: %s, Error: %v\n", qa.AgentId, qa.DeploymentName, err)

	} else {
		res := &ClusterMetricsList{}
		unmarshalerr := json.Unmarshal(data, res)
		if unmarshalerr != nil {
			glog.V(2).Infof("Failed to unmarshal metrics to struct: %v from deployment Agent ID: %s with Agent QueueJob Name: %s, Error: %v\n",
				res, qa.AgentId, qa.DeploymentName, unmarshalerr)
		} else {
			if len(res.Items) > 0 {
				for i := 0; i < len(res.Items); i++ {
					glog.V(9).Infof("Obtained the metric:%s, label:%v, value: %s, from the Agent: %s  with Agent QueueJob Name: %s.\n",
						res.Items[i].MetricName, res.Items[i].MetricLabels, res.Items[i].Value, qa.AgentId, qa.DeploymentName)
					clusterMetricType := res.Items[i].MetricLabels["cluster"]
					if strings.Compare(clusterMetricType, "cpu") == 0  || strings.Compare(clusterMetricType, "memory") == 0 {
						num, err := strconv.ParseFloat(res.Items[i].Value, 64)
						if err !=nil {
							glog.Warningf("Possible issue converting %s string value of %s due to error: %v\n",
								clusterMetricType, res.Items[i].Value, err)
						} else {
							f_num := math.Float64bits(num)
							f_zero := math.Float64bits(0.0)
							if (f_num > f_zero) {
								if strings.Compare(clusterMetricType, "cpu") == 0 {
									qa.AggrResources.MilliCPU = num
									glog.V(10).Infof("Updated %s from %f to %f for metrics: %v from deployment Agent ID: %s with Agent QueueJob Name: %s\n",
										clusterMetricType, qa.AggrResources.MilliCPU, num, res, qa.AgentId, qa.DeploymentName)
								} else {
									qa.AggrResources.Memory = num
									glog.V(10).Infof("Updated %s from %f to %f for metrics: %v from deployment Agent ID: %s with Agent QueueJob Name: %s\n",
										clusterMetricType, qa.AggrResources.Memory, num, res, qa.AgentId, qa.DeploymentName)
								}
							} else {
								glog.Warningf("Possible issue converting %s string value of %s to float type.  Conversion result: %f\n",
									clusterMetricType, res.Items[i].Value, num)
							}  // Float value resulted in zero value.
						} // Converting string to float success
					} else {
						glog.V(9).Infof("Unknown label value: %s for metrics: %v from deployment Agent ID: %s with Agent QueueJob Name: %s\n",
							clusterMetricType, res, qa.AgentId, qa.DeploymentName)
					} // Unknown label

				}
			} else {
				glog.V(2).Infof("Failed to obtain values for metrics: %v from deployment Agent ID: %s with Agent QueueJob Name: %s, Error: %v\n", res, qa.AgentId, qa.DeploymentName, unmarshalerr)
			}
		}
	}

	glog.V(4).Infof("[Dispatcher: Agent] Updated Aggr Resources of %s: %v\n", qa.AgentId, qa.AggrResources)
    return nil
}

func buildResource(cpu string, memory string) *clusterstateapi.Resource {
    return clusterstateapi.NewResource(v1.ResourceList{
        v1.ResourceCPU:    resource.MustParse(cpu),
        v1.ResourceMemory: resource.MustParse(memory),
    })
}
