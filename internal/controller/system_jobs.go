/*
Copyright The Zabbix Operator Authors.

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

package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sagh0900/zabbix-operator/internal/jobs"
)

// retryFailedJobAfter is how long a failed Job stays before it is deleted and run again,
// so a blocked precheck is re-evaluated after the cause is fixed.
const retryFailedJobAfter = time.Minute

// runJob makes sure the Job for spec exists and returns its result once it has finished;
// nil means it is still running.
func (r *SystemReconciler) runJob(ctx context.Context, spec jobs.Spec) (*jobs.Result, error) {
	want, err := jobs.Build(spec, r.Scheme)
	if err != nil {
		return nil, err
	}
	job := &batchv1.Job{}
	err = r.Get(ctx, client.ObjectKeyFromObject(want), job)
	switch {
	case apierrors.IsNotFound(err):
		return nil, client.IgnoreAlreadyExists(r.Create(ctx, want))
	case err != nil:
		return nil, err
	}

	finished, failed, at := jobFinished(job)
	if !finished {
		return nil, nil
	}
	res, err := r.jobResult(ctx, job)
	if err != nil {
		return nil, err
	}
	if failed && res == nil {
		res = &jobs.Result{Command: spec.Command, Reason: "JobFailed", Message: "the " + spec.Command + " Job failed without a result"}
	}
	if res != nil && !res.OK && r.now().Sub(at) > retryFailedJobAfter {
		if err := r.Delete(ctx, job, client.PropagationPolicy("Background")); client.IgnoreNotFound(err) != nil {
			return nil, err
		}
	}
	return res, nil
}

func jobFinished(job *batchv1.Job) (finished, failed bool, at time.Time) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, false, c.LastTransitionTime.Time
		case batchv1.JobFailed:
			return true, true, c.LastTransitionTime.Time
		}
	}
	return false, false, time.Time{}
}

// jobResult reads the result a Job's pod wrote to its termination message.
func (r *SystemReconciler) jobResult(ctx context.Context, job *batchv1.Job) (*jobs.Result, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return nil, err
	}
	var latest *jobs.Result
	var latestAt time.Time
	for _, p := range pods.Items {
		for _, c := range p.Status.ContainerStatuses {
			t := c.State.Terminated
			if c.Name != "job" || t == nil || t.Message == "" {
				continue
			}
			res, err := jobs.ParseResult(t.Message)
			if err != nil {
				return nil, fmt.Errorf("job %s: %w", job.Name, err)
			}
			if latest == nil || t.FinishedAt.After(latestAt) {
				latest, latestAt = &res, t.FinishedAt.Time
			}
		}
	}
	return latest, nil
}
