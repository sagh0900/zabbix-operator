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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/jobs"
	"github.com/sagh0900/zabbix-operator/internal/metrics"
)

// retryFailedJobAfter is how long a failed Job stays before it is deleted and run again,
// so a blocked precheck is re-evaluated after the cause is fixed.
const retryFailedJobAfter = time.Minute

// jobCleanup says what happens to a finished Job.
type jobCleanup int

const (
	// retryFailed keeps a successful Job (its result answers repeated questions) and
	// deletes a failed one after retryFailedJobAfter, so the next pass runs it again.
	retryFailed jobCleanup = iota
	// keepFinished never deletes a finished Job: a failed one stays, with its pods, for
	// inspection until its TTL. For periodic Jobs whose next run is a new Job anyway; their
	// successful runs are removed by the caller once the result is recorded.
	keepFinished
)

// runJob makes sure the Job for spec exists and returns its result once it has finished;
// nil means it is still running. Finished Jobs are cleaned up with retryFailed.
func (r *SystemReconciler) runJob(ctx context.Context, spec jobs.Spec) (*jobs.Result, error) {
	res, _, err := r.runJobWith(ctx, spec, retryFailed)
	return res, err
}

// runJobWith is runJob with a cleanup policy. first is true the first time a result of
// this Job is returned.
func (r *SystemReconciler) runJobWith(ctx context.Context, spec jobs.Spec, cleanup jobCleanup) (res *jobs.Result, first bool, err error) {
	// Jobs run the operator image; they pull it with the server's pull secrets, which
	// cover the usual case of one private registry or mirror for every image.
	if sys, ok := spec.Owner.(*zabbixv1alpha1.ZabbixSystem); ok && spec.ImagePullSecrets == nil {
		spec.ImagePullSecrets = sys.Spec.Server.ImagePullSecrets
	}
	want, err := jobs.Build(spec, r.Scheme)
	if err != nil {
		return nil, false, err
	}
	job := &batchv1.Job{}
	err = r.Get(ctx, client.ObjectKeyFromObject(want), job)
	switch {
	case apierrors.IsNotFound(err):
		return nil, false, client.IgnoreAlreadyExists(r.Create(ctx, want))
	case err != nil:
		return nil, false, err
	}

	finished, failed, at := jobFinished(job)
	if !finished {
		return nil, false, nil
	}
	if res, err = r.jobResult(ctx, job); err != nil {
		return nil, false, err
	}
	if failed && res == nil {
		res = &jobs.Result{Command: spec.Command, Reason: "JobFailed", Message: "the " + spec.Command + " Job failed without a result"}
	}
	if res != nil {
		if _, counted := r.countedJobs.LoadOrStore(job.UID, true); !counted {
			first = true
			result := "succeeded"
			if !res.OK {
				result = "failed"
			}
			metrics.JobRuns.WithLabelValues(spec.Owner.GetNamespace(), spec.System, spec.Command, result).Inc()
		}
	}
	if removeFinishedJob(cleanup, res, r.now().Sub(at)) {
		if err := r.Delete(ctx, job, client.PropagationPolicy("Background")); client.IgnoreNotFound(err) != nil {
			return nil, false, err
		}
	}
	return res, first, nil
}

// removeFinishedJob decides whether a finished Job with result res, finished age ago, is
// deleted under the cleanup policy.
func removeFinishedJob(cleanup jobCleanup, res *jobs.Result, age time.Duration) bool {
	switch {
	case res == nil:
		return false
	case cleanup == keepFinished:
		return false
	default:
		return !res.OK && age > retryFailedJobAfter
	}
}

// deleteJob removes the Job for spec, so the next runJob starts it again.
func (r *SystemReconciler) deleteJob(ctx context.Context, spec jobs.Spec) error {
	job, err := jobs.Build(spec, r.Scheme)
	if err != nil {
		return err
	}
	return client.IgnoreNotFound(r.Delete(ctx, job, client.PropagationPolicy("Background")))
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

// jobResult reads the result a Job's pod wrote to its termination message. Only pods
// controlled by this Job count: a Job recreated under the same name must never read a
// result left by the pods of its predecessor before they are garbage-collected.
func (r *SystemReconciler) jobResult(ctx context.Context, job *batchv1.Job) (*jobs.Result, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return nil, err
	}
	var latest *jobs.Result
	var latestAt time.Time
	for _, p := range pods.Items {
		if ref := metav1.GetControllerOf(&p); ref == nil || ref.UID != job.UID {
			continue
		}
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
