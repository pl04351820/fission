/*
Copyright 2016 The Fission Authors.

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

package executor

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/fission/fission/crd"
	"github.com/fission/fission/executor/fscache"

	"github.com/fission/fission"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// cleanupService cleans up resources created by old backend instances
// and reaps idle resources based on minScale parameter
func cleanupService(kubernetesClient *kubernetes.Clientset,
	fissionClient *crd.FissionClient,
	fsCache *fscache.FunctionServiceCache,
	namespace string,
	instanceId string) {
	go func() {
		err := cleanup(kubernetesClient, namespace, instanceId)
		if err != nil {
			// TODO retry cleanup; logged and ignored for now
			log.Printf("Failed to cleanup: %v", err)
		}
	}()

	go idleObjectReaper(kubernetesClient, fissionClient, fsCache, time.Minute*2)
}

func cleanup(client *kubernetes.Clientset, namespace string, instanceId string) error {

	err := cleanupServices(client, namespace, instanceId)
	if err != nil {
		return err
	}

	err = cleanupHpa(client, namespace, instanceId)
	if err != nil {
		return err
	}

	// Deployments are used for idle pools and can be cleaned up
	// immediately.  (We should "adopt" these instead of creating
	// a new pool.)
	err = cleanupDeployments(client, namespace, instanceId)
	if err != nil {
		return err
	}
	// See K8s #33845 and related bugs: deleting a deployment
	// through the API doesn't cause the associated ReplicaSet to
	// be deleted.  (Fixed recently, but we may be running a
	// version before the fix.)
	err = cleanupReplicaSets(client, namespace, instanceId)
	if err != nil {
		return err
	}

	// Pods might still be running user functions, so we give them
	// a few minutes before terminating them.  This time is the
	// maximum function runtime, plus the time a router might
	// still route to an old instance, i.e. router cache expiry
	// time.
	time.Sleep(6 * time.Minute)

	err = cleanupPods(client, namespace, instanceId)
	if err != nil {
		return err
	}

	return nil
}

func idleObjectReaper(kubeClient *kubernetes.Clientset,
	fissionClient *crd.FissionClient,
	fsCache *fscache.FunctionServiceCache,
	idlePodReapTime time.Duration) {

	pollSleep := time.Duration(2 * time.Second)
	for {
		time.Sleep(pollSleep)

		envs, err := fissionClient.Environments(meta_v1.NamespaceAll).List(meta_v1.ListOptions{})
		if err != nil {
			log.Fatalf("Failed to get environment list: %v", err)
		}

		for i := range envs.Items {
			env := envs.Items[i]
			if env.Spec.AllowedFunctionsPerContainer != fission.AllowedFunctionsPerContainerInfinite {
				funcSvcs, err := fsCache.ListOld(&env.Metadata, idlePodReapTime)
				if err != nil {
					log.Printf("Error reaping idle pods: %v", err)
					continue
				}

				for _, fsvc := range funcSvcs {

					fn, err := fissionClient.Functions(fsvc.Function.Namespace).Get(fsvc.Function.Name)
					if err != nil {
						log.Printf("Error getting function: %v", fsvc.Function.Name)
						continue
					}

					// Ignore functions of NewDeploy backend with MinScale > 0
					if !(fn.Spec.InvokeStrategy.ExecutionStrategy.MinScale > 0 && fn.Spec.InvokeStrategy.ExecutionStrategy.Backend == fission.BackendTypeNewdeploy) {
						deleted, err := fsCache.DeleteOld(fsvc, idlePodReapTime)

						if err != nil {
							log.Printf("Error deleting Kubernetes objects for fsvc '%v': %v", fsvc, err)
							log.Printf("Object Name| Object Kind | Object Namespace")
							for _, kubeobj := range fsvc.KubernetesObjects {
								log.Printf("%v | %v | %v", kubeobj.Name, kubeobj.Kind, kubeobj.Namespace)
							}
						}

						if !deleted {
							log.Printf("Not deleting %v, in use", fsvc.Function)
						} else {
							for _, kubeobj := range fsvc.KubernetesObjects {
								switch strings.ToLower(kubeobj.Kind) {
								case "pod":
									err = kubeClient.CoreV1().Pods(kubeobj.Namespace).Delete(kubeobj.Name, nil)
									logErr(fmt.Sprintf("cleaning up pod %v ", kubeobj.Name), err)
								case "service":
									err = kubeClient.CoreV1().Services(kubeobj.Namespace).Delete(kubeobj.Name, nil)
									logErr(fmt.Sprintf("cleaning up service %v ", kubeobj.Name), err)
								case "deployment":
									err = kubeClient.ExtensionsV1beta1().Deployments(kubeobj.Namespace).Delete(kubeobj.Name, nil)
									logErr(fmt.Sprintf("cleaning up deployment %v ", kubeobj.Name), err)
								case "horizontalpodautoscaler":
									err = kubeClient.AutoscalingV1().HorizontalPodAutoscalers(kubeobj.Namespace).Delete(kubeobj.Name, nil)
									logErr(fmt.Sprintf("cleaning up horizontalpodautoscaler %v ", kubeobj.Name), err)
								default:
									log.Printf("There was an error identifying the object type: %v for obj: %v", kubeobj.Kind, kubeobj)
								}
							}
						}
					}
				}
			}
		}
	}
}

func cleanupDeployments(client *kubernetes.Clientset, namespace string, instanceId string) error {
	deploymentList, err := client.ExtensionsV1beta1().Deployments(namespace).List(meta_v1.ListOptions{})
	if err != nil {
		return err
	}
	for _, dep := range deploymentList.Items {
		id, ok := dep.ObjectMeta.Labels[fission.EXECUTOR_INSTANCEID_LABEL]
		if ok && id != instanceId {
			log.Printf("Cleaning up deployment %v", dep.ObjectMeta.Name)
			err := client.ExtensionsV1beta1().Deployments(namespace).Delete(dep.ObjectMeta.Name, nil)
			logErr("cleaning up deployment", err)
			// ignore err
		}
	}
	return nil
}

func cleanupReplicaSets(client *kubernetes.Clientset, namespace string, instanceId string) error {
	rsList, err := client.ExtensionsV1beta1().ReplicaSets(namespace).List(meta_v1.ListOptions{})
	if err != nil {
		return err
	}
	for _, rs := range rsList.Items {
		id, ok := rs.ObjectMeta.Labels[fission.EXECUTOR_INSTANCEID_LABEL]
		if ok && id != instanceId {
			log.Printf("Cleaning up replicaset %v", rs.ObjectMeta.Name)
			err := client.ExtensionsV1beta1().ReplicaSets(namespace).Delete(rs.ObjectMeta.Name, nil)
			logErr("cleaning up replicaset", err)
		}
	}
	return nil
}

func cleanupPods(client *kubernetes.Clientset, namespace string, instanceId string) error {
	podList, err := client.CoreV1().Pods(namespace).List(meta_v1.ListOptions{})
	if err != nil {
		return err
	}
	for _, pod := range podList.Items {
		id, ok := pod.ObjectMeta.Labels[fission.EXECUTOR_INSTANCEID_LABEL]
		if ok && id != instanceId {
			log.Printf("Cleaning up pod %v", pod.ObjectMeta.Name)
			err := client.CoreV1().Pods(namespace).Delete(pod.ObjectMeta.Name, nil)
			logErr("cleaning up pod", err)
			// ignore err
		}
	}
	return nil
}

func cleanupServices(client *kubernetes.Clientset, namespace string, instanceId string) error {
	svcList, err := client.CoreV1().Services(namespace).List(meta_v1.ListOptions{})
	if err != nil {
		return err
	}
	for _, svc := range svcList.Items {
		id, ok := svc.ObjectMeta.Labels[fission.EXECUTOR_INSTANCEID_LABEL]
		if ok && id != instanceId {
			log.Printf("Cleaning up svc %v", svc.ObjectMeta.Name)
			err := client.CoreV1().Services(namespace).Delete(svc.ObjectMeta.Name, nil)
			logErr("cleaning up service", err)
			// ignore err
		}
	}
	return nil
}

func cleanupHpa(client *kubernetes.Clientset, namespace string, instanceId string) error {
	hpaList, err := client.AutoscalingV1().HorizontalPodAutoscalers(namespace).List(meta_v1.ListOptions{})
	if err != nil {
		return err
	}

	for _, hpa := range hpaList.Items {
		id, ok := hpa.ObjectMeta.Labels[fission.EXECUTOR_INSTANCEID_LABEL]
		if ok && id != instanceId {
			log.Printf("Cleaning up HPA %v", hpa.ObjectMeta.Name)
			err := client.AutoscalingV1().HorizontalPodAutoscalers(namespace).Delete(hpa.ObjectMeta.Name, nil)
			logErr("cleaning up HPA", err)
		}

	}
	return nil

}

func logErr(msg string, err error) {
	if err != nil {
		log.Printf("Error %v: %v", msg, err)
	}
}