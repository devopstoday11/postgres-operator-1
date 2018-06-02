// Package cluster holds the cluster CRD logic and definitions
// A cluster is comprised of a primary service, replica service,
// primary deployment, and replica deployment
package cluster

/*
 Copyright 2017-2018 Crunchy Data Solutions, Inc.
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

import (
	log "github.com/Sirupsen/logrus"
	crv1 "github.com/crunchydata/postgres-operator/apis/cr/v1"
	"github.com/crunchydata/postgres-operator/kubeapi"
	"github.com/crunchydata/postgres-operator/util"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"time"
)

// StateMachine holds a state machine that is created when
// a cluster has received a NotReady event, this is the start
// The StateMachine is executed in a separate goroutine for
// any cluster founds to be NotReady
type StateMachine struct {
	Clientset    *kubernetes.Clientset
	RESTClient   *rest.RESTClient
	Namespace    string
	ClusterName  string
	SleepSeconds int
}

// FailoverEvent holds a record of a NotReady or other event that
// is used by the failover algorithm, FailoverEvents can build up
// for a given cluster
type FailoverEvent struct {
	EventType string
	EventTime time.Time
}

const FAILOVER_EVENT_NOT_READY = "NotReady"
const FAILOVER_EVENT_READY = "Ready"

type AutoFailoverTask struct {
}

func InitializeAutoFailover(clientset *kubernetes.Clientset, restclient *rest.RESTClient, ns string) {
	aftask := AutoFailoverTask{}

	log.Infoln("autofailover Initialize ")

	pods, _ := kubeapi.GetPods(clientset, util.LABEL_AUTOFAIL, ns)
	log.Infof("%d autofail pods found\n", len(pods.Items))

	for _, p := range pods.Items {
		clusterName := p.ObjectMeta.Labels[util.LABEL_PG_CLUSTER]
		for _, c := range p.Status.ContainerStatuses {
			if c.Name == "database" {
				if c.Ready {
					aftask.AddEvent(restclient, clusterName, FAILOVER_EVENT_READY, ns)
				} else {
					aftask.AddEvent(restclient, clusterName, FAILOVER_EVENT_NOT_READY, ns)
					sm := StateMachine{
						Clientset:    clientset,
						RESTClient:   restclient,
						Namespace:    ns,
						SleepSeconds: 9,
						ClusterName:  clusterName,
					}

					go sm.Run()

				}
			}
		}
	}

	aftask.Print(restclient, ns)
}

func (s *StateMachine) Print() {

	log.Debugf("StateMachine: %s sleeping %d\n", s.ClusterName, s.SleepSeconds)
}

// Evaluate returns true if the autofail status is NotReady
func (s *StateMachine) Evaluate(status string, events map[string]string) bool {

	if status == FAILOVER_EVENT_NOT_READY {
		log.Debugf("Failover scenario caught: NotReady for %s\n", s.ClusterName)
		return true
	}

	return false
}

// Run is the heart of the failover state machine, started when a NotReady
// event is caught by the cluster watcher process, this statemachine
// runs until
func (s *StateMachine) Run() {

	aftask := AutoFailoverTask{}

	for {
		time.Sleep(time.Second * time.Duration(s.SleepSeconds))
		s.Print()

		status, events := aftask.GetEvents(s.RESTClient, s.ClusterName, s.Namespace)
		if len(events) == 0 {
			log.Debugf("no events for statemachine, exiting")
			return
		}

		failoverRequired := s.Evaluate(status, events)
		if failoverRequired {
			log.Infof("failoverRequired is true, trigger failover on %s\n", s.ClusterName)
			s.triggerFailover()
			//clean up to not reprocess the failover event
			aftask.Clear(s.RESTClient, s.ClusterName, s.Namespace)
		} else {
			log.Infof("failoverRequired is false, no need to trigger failover\n")
		}

		//right now, there is no need for looping with this
		//simple failover check algorithm, later this loop
		//will be necessary potentially if the logic evaluates
		//failures over a span of time
		return

	}

}

// AutofailBase ...
func AutofailBase(clientset *kubernetes.Clientset, restclient *rest.RESTClient, ready bool, clusterName, namespace string) {
	log.Infof("AutofailBase ready=%v cluster=%s namespace=%s\n", ready, clusterName, namespace)

	aftask := AutoFailoverTask{}

	exists := aftask.Exists(restclient, clusterName, namespace)
	if exists {
		if !ready {
			//add notready event, start a state machine
			aftask.AddEvent(restclient, clusterName, FAILOVER_EVENT_NOT_READY, namespace)
			//create a state machine to track the failovers for test cluster
			sm := StateMachine{
				Clientset:    clientset,
				RESTClient:   restclient,
				Namespace:    namespace,
				SleepSeconds: 9,
				ClusterName:  clusterName,
			}

			go sm.Run()

		}

	} else {
		//we only register the autofail target once it
		//goes into a Ready status for the first time
		if ready {
			//add new map entry to keep an eye on it
			log.Infof("adding ready failover event for %s\n", clusterName)
			aftask.AddEvent(restclient, clusterName, FAILOVER_EVENT_READY, namespace)
		}
	}

	/**
	cluster := crv1.Pgcluster{}
	_, err = kubeapi.Getpgcluster(client, &cluster,
		clusterName, namespace)
	if err != nil {
		return
	}

	if cluster.Spec.Strategy == "" {
		cluster.Spec.Strategy = "1"
		log.Info("using default strategy")
	}

	strategy, ok := strategyMap[cluster.Spec.Strategy]
	if ok {
		log.Info("strategy found")
	} else {
		log.Error("invalid Strategy requested for cluster failover " + cluster.Spec.Strategy)
		return
	}

	strategy.Failover(clientset, client, clusterName, task, namespace, restconfig)
	*/

}

func (*AutoFailoverTask) Exists(restclient *rest.RESTClient, clusterName, namespace string) bool {
	task := crv1.Pgtask{}
	taskName := clusterName + "-" + util.LABEL_AUTOFAIL
	found, _ := kubeapi.Getpgtask(restclient, &task, taskName, namespace)
	return found
}

func (*AutoFailoverTask) AddEvent(restclient *rest.RESTClient, clusterName, eventType, namespace string) {
	var err error
	var found bool

	taskName := clusterName + "-" + util.LABEL_AUTOFAIL
	task := crv1.Pgtask{}
	found, err = kubeapi.Getpgtask(restclient, &task, taskName, namespace)
	if !found {
		task.Name = taskName
		task.Spec.Status = eventType
		task.Spec.Name = clusterName
		task.ObjectMeta.Labels = make(map[string]string)
		task.ObjectMeta.Labels[util.LABEL_AUTOFAIL] = "true"
		task.Spec.TaskType = crv1.PgtaskAutoFailover
		task.Spec.Parameters = make(map[string]string)
		task.Spec.Parameters[time.Now().String()] = eventType
		err = kubeapi.Createpgtask(restclient, &task, namespace)
		return
	}

	task.Spec.Status = eventType
	task.Spec.Parameters[time.Now().String()] = eventType
	err = kubeapi.Updatepgtask(restclient, &task, taskName, namespace)
	if err != nil {
		log.Error(err)
	}

}

func (*AutoFailoverTask) Print(restclient *rest.RESTClient, namespace string) {

	log.Infoln("AutoFail pgtask List....")

	tasklist := crv1.PgtaskList{}

	err := kubeapi.GetpgtasksBySelector(restclient, &tasklist, util.LABEL_AUTOFAIL, namespace)
	if err != nil {
		log.Error(err)
		return

	}
	for k, v := range tasklist.Items {
		log.Infof("k=%s v=%v tasktype=%s\n", k, v.Name, v.Spec.TaskType)
		for x, y := range v.Spec.Parameters {
			log.Infof("parameter %s %s\n", x, y)
		}
	}

}

func (*AutoFailoverTask) Clear(restclient *rest.RESTClient, clusterName, namespace string) {

	taskName := clusterName + "-" + util.LABEL_AUTOFAIL
	kubeapi.Deletepgtask(restclient, taskName, namespace)
}

func (*AutoFailoverTask) GetEvents(restclient *rest.RESTClient, clusterName, namespace string) (string, map[string]string) {
	task := crv1.Pgtask{}
	taskName := clusterName + "-" + util.LABEL_AUTOFAIL
	found, _ := kubeapi.Getpgtask(restclient, &task, taskName, namespace)
	if found {
		return task.Spec.Status, task.Spec.Parameters
	}
	return "", make(map[string]string)
}

func getTargetDeployment(clientset *kubernetes.Clientset, clusterName, ns string) (string, error) {

	selector := util.LABEL_PRIMARY + "=false," + util.LABEL_PG_CLUSTER + "=" + clusterName

	deployments, err := kubeapi.GetDeployments(clientset, selector, ns)
	if kerrors.IsNotFound(err) {
		log.Debug("no replicas found ")
		return "", err
	} else if err != nil {
		log.Error("error getting deployments " + err.Error())
		return "", err
	}

	//for now, just return the first replica, in the future
	//return the most up to date
	log.Debugf("deps len %d\n", len(deployments.Items))
	for _, dep := range deployments.Items {
		log.Debug("found " + dep.Name)
		return dep.Name, err
	}

	return "", err

}

func (s *StateMachine) triggerFailover() {
	targetDeploy, err := getTargetDeployment(s.Clientset, s.ClusterName, s.Namespace)
	if targetDeploy == "" || err != nil {
		log.Errorf("could not autofailover with no replicas found for %s\n", s.ClusterName)
		return
	}

	spec := crv1.PgtaskSpec{}
	spec.Name = s.ClusterName + "-" + util.LABEL_FAILOVER
	kubeapi.Deletepgtask(s.RESTClient, s.ClusterName, s.Namespace)
	spec.TaskType = crv1.PgtaskFailover
	spec.Parameters = make(map[string]string)
	spec.Parameters[s.ClusterName] = s.ClusterName
	labels := make(map[string]string)
	labels[util.LABEL_TARGET] = targetDeploy
	labels[util.LABEL_PG_CLUSTER] = s.ClusterName
	newInstance := &crv1.Pgtask{
		ObjectMeta: meta_v1.ObjectMeta{
			Name:   spec.Name,
			Labels: labels,
		},
		Spec: spec,
	}

	err = kubeapi.Createpgtask(s.RESTClient,
		newInstance, s.Namespace)
	if err != nil {
		log.Error(err)
		log.Error("could not create pgtask for autofailover failover task")
	} else {
		log.Infof("created pgtask failover by autofailover %s\n", s.ClusterName)
	}

}
