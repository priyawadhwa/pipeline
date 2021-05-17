// +build e2e

/*
Copyright 2021 The Tekton Authors

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

package test

import (
	"context"
	"testing"
	"time"

	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHermeticTaskRun(t *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c, namespace := setup(ctx, t)
	t.Parallel()
	defer tearDown(ctx, t, c, namespace)

	// first, run the task run with hermetic=false to prove that it succeeds
	regularTaskRunName := "not-hermetic"
	regularTaskRun := taskRun(regularTaskRunName, namespace, false)
	t.Logf("Creating TaskRun %s, hermetic=false", regularTaskRunName)
	if _, err := c.TaskRunClient.Create(ctx, regularTaskRun, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Failed to create TaskRun `%s`: %s", regularTaskRunName, err)
	}
	if err := WaitForTaskRunState(ctx, c, regularTaskRunName, Succeed(regularTaskRunName), "TaskRunCompleted"); err != nil {
		t.Fatalf("Error waiting for TaskRun %s to finish: %s", regularTaskRunName, err)
	}

	// now, run the task mode with hermetic mode
	// it should fail, since it shouldn't be able to access any network
	hermeticTaskRunName := "hermetic-should-fail"
	hermeticTaskRun := taskRun(hermeticTaskRunName, namespace, true)
	t.Logf("Creating TaskRun %s, hermetic=true", hermeticTaskRunName)
	if _, err := c.TaskRunClient.Create(ctx, hermeticTaskRun, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Failed to create TaskRun `%s`: %s", regularTaskRun.Name, err)
	}
	if err := WaitForTaskRunState(ctx, c, hermeticTaskRunName, Failed(hermeticTaskRunName), "Failed"); err != nil {
		t.Fatalf("Error waiting for TaskRun %s to fail: %s", hermeticTaskRunName, err)
	}
}

func taskRun(name, namespace string, hermetic bool) *v1beta1.TaskRun {
	return &v1beta1.TaskRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1beta1.TaskRunSpec{
			ExecutionMode: v1beta1.ExecutionMode{
				Hermetic: hermetic,
			},
			Timeout: &metav1.Duration{Duration: time.Minute},
			TaskSpec: &v1beta1.TaskSpec{
				Steps: []v1beta1.Step{
					{
						Container: corev1.Container{
							Name:  "access-network",
							Image: "ubuntu",
						},
						Script: `#!/bin/bash
set -ex
apt-get update
apt-get install -y curl`,
					},
				},
			},
		},
	}
}
