/*
Copyright 2022.

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

package configcheck

import (
	"context"
	"math/rand"
	"time"

	"github.com/kaasops/vector-operator/controllers/factory/utils/k8s"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type ConfigCheck struct {
	Config []byte

	Client    client.Client
	ClientSet *kubernetes.Clientset

	Name      string
	Namespace string
	Image     string
	Envs      []corev1.EnvVar
	Hash      string
}

func New(
	config []byte,
	c client.Client,
	cs *kubernetes.Clientset,
	name, namespace, image string,
	envs []corev1.EnvVar,
) *ConfigCheck {
	return &ConfigCheck{
		Config:    config,
		Client:    c,
		ClientSet: cs,
		Name:      name,
		Namespace: namespace,
		Image:     image,
		Envs:      envs,
	}
}

func (cc *ConfigCheck) Run(ctx context.Context) error {
	log := log.FromContext(ctx).WithValues("Vector ConfigCheck", cc.Name)

	log.Info("start ConfigCheck")

	if err := cc.ensureVectorConfigCheckRBAC(ctx); err != nil {
		return err
	}

	cc.Hash = randStringRunes()

	if err := cc.ensureVectorConfigCheckConfig(ctx); err != nil {
		return err
	}

	if err := cc.checkVectorConfigCheckPod(ctx); err != nil {
		return err
	}

	return nil
}

func (cc *ConfigCheck) ensureVectorConfigCheckRBAC(ctx context.Context) error {
	return cc.ensureVectorConfigCheckServiceAccount(ctx)
}

func (cc *ConfigCheck) ensureVectorConfigCheckServiceAccount(ctx context.Context) error {
	vectorAgentServiceAccount := cc.createVectorConfigCheckServiceAccount()

	return k8s.CreateOrUpdateResource(ctx, vectorAgentServiceAccount, cc.Client)
}
func (cc *ConfigCheck) ensureVectorConfigCheckConfig(ctx context.Context) error {
	vectorConfigCheckSecret, err := cc.createVectorConfigCheckConfig()
	if err != nil {
		return err
	}

	return k8s.CreateOrUpdateResource(ctx, vectorConfigCheckSecret, cc.Client)
}

func (cc *ConfigCheck) checkVectorConfigCheckPod(ctx context.Context) error {
	vectorConfigCheckPod := cc.createVectorConfigCheckPod()

	err := k8s.CreatePod(ctx, vectorConfigCheckPod, cc.Client)
	if err != nil {
		return err
	}

	err = cc.getCheckResult(ctx, vectorConfigCheckPod)
	if err != nil {
		return err
	}

	err = cc.cleanup(ctx)
	if err != nil {
		return err
	}
	return nil
}

func labelsForVectorConfigCheck() map[string]string {
	return map[string]string{
		k8s.ManagedByLabelKey:  "vector-operator",
		k8s.NameLabelKey:       "vector-configcheck",
		k8s.ComponentLabelKey:  "ConfigCheck",
		k8s.VectorExcludeLabel: "true",
	}
}

func (cc *ConfigCheck) getNameVectorConfigCheck() string {
	n := "configcheck-" + "-" + cc.Name + "-" + cc.Hash

	return n
}

func randStringRunes() string {
	var letterRunes = []rune("abcdefghijklmnopqrstuvwxyz")

	b := make([]rune, 5)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

func (cc *ConfigCheck) getCheckResult(ctx context.Context, pod *corev1.Pod) error {
	log := log.FromContext(ctx).WithValues("Vector ConfigCheck", pod.Name)

	for {
		existing, err := k8s.GetPod(ctx, client.ObjectKeyFromObject(pod), cc.Client)
		if err != nil {
			return err
		}

		switch existing.Status.Phase {
		case "Pending":
			log.Info("wait Validate Vector Config Result")
			time.Sleep(5 * time.Second)
		case "Failed":
			reason, err := k8s.GetPodLogs(ctx, pod, cc.ClientSet)
			if err != nil {
				return err
			}
			return newValidationError(reason)
		case "Succeeded":
			log.Info("Config Check completed successfully")
			return nil
		}
	}
}

func (cc *ConfigCheck) cleanup(ctx context.Context) error {
	listOpts, err := cc.gcRListOptions()
	if err != nil {
		return err
	}

	podlist := corev1.PodList{}
	err = cc.Client.List(ctx, &podlist, &listOpts)
	if err != nil {
		return err
	}
	for _, pod := range podlist.Items {
		if pod.Status.Phase == "Succeeded" {
			for _, v := range pod.Spec.Volumes {
				if v.Name == "config" {
					nn := types.NamespacedName{
						Name:      v.Secret.SecretName,
						Namespace: pod.Namespace,
					}
					secret, err := k8s.GetSecret(ctx, nn, cc.Client)
					if err != nil {
						return err
					}
					if err := k8s.DeleteSecret(ctx, secret, cc.Client); err != nil {
						return err
					}
				}
			}
			if err := k8s.DeletePod(ctx, &pod, cc.Client); err != nil {
				return err
			}
		}
	}
	return nil
}

func (cc *ConfigCheck) gcRListOptions() (client.ListOptions, error) {
	configCheckLabels := labelsForVectorConfigCheck()
	var requirements []labels.Requirement
	for k, v := range configCheckLabels {
		r, err := labels.NewRequirement(k, "==", []string{v})
		if err != nil {
			return client.ListOptions{}, err
		}
		requirements = append(requirements, *r)
	}
	labelsSelector := labels.NewSelector().Add(requirements...)

	return client.ListOptions{
		LabelSelector: labelsSelector,
		Namespace:     cc.Namespace,
	}, nil
}
