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

package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/podset"
)

// LabelJob carries the command a Job runs.
const LabelJob = "zabbix.io/job"

// nonRootUID is the user of the distroless operator image.
const nonRootUID = 65532

// Spec describes one Job.
type Spec struct {
	// Owner controls the Job; deleting it deletes the Job.
	Owner client.Object
	// System names the ZabbixSystem the Job works for.
	System string
	// Command is precheck, ha-reset or ha-gc; Args are its flags.
	Command string
	Args    []string
	// Image is the operator image, which contains the job runner.
	Image string
	// Database is the database to connect to; Host is used (the direct host for schema work).
	Database *zabbixv1alpha1.ZabbixDatabase
	Host     string
	// RunID distinguishes repeated runs with identical arguments, such as periodic ha-gc.
	RunID string
}

// Name returns a deterministic Job name: the same inputs always give the same Job, so
// reconciling is idempotent, and changed inputs give a new Job.
func Name(s Spec) string {
	h := sha256.New()
	for _, part := range append([]string{s.Command, s.Image, s.Host, s.RunID}, s.Args...) {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	suffix := "-" + s.Command + "-" + hex.EncodeToString(h.Sum(nil))[:8]
	prefix := s.System
	if max := 63 - len(suffix); len(prefix) > max {
		prefix = strings.TrimRight(prefix[:max], "-")
	}
	return prefix + suffix
}

// Build returns the Job for s, owned by s.Owner.
func Build(s Spec, scheme *runtime.Scheme) (*batchv1.Job, error) {
	db := s.Database
	userKey, passKey := db.Spec.CredentialsRef.UsernameKey, db.Spec.CredentialsRef.PasswordKey
	if userKey == "" {
		userKey = "username"
	}
	if passKey == "" {
		passKey = "password"
	}
	port := db.Spec.Port
	if port == 0 {
		port = 5432
	}
	name := db.Spec.Database
	if name == "" {
		name = "zabbix"
	}

	labels := map[string]string{
		podset.LabelManagedBy: podset.ManagedBy,
		podset.LabelSystem:    s.System,
		LabelJob:              s.Command,
	}
	env := []corev1.EnvVar{
		{Name: EnvHost, Value: s.Host},
		{Name: EnvPort, Value: strconv.Itoa(int(port))},
		{Name: EnvName, Value: name},
		{Name: EnvSSLMode, Value: "prefer"},
	}
	volumes := []corev1.Volume{{
		Name: "credentials",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  db.Spec.CredentialsRef.SecretName,
			Items:       []corev1.KeyToPath{{Key: userKey, Path: "username"}, {Key: passKey, Path: "password"}},
			DefaultMode: ptr.To[int32](0o440),
		}},
	}}
	mounts := []corev1.VolumeMount{{Name: "credentials", MountPath: CredentialsDir, ReadOnly: true}}

	if tls := db.Spec.TLS; tls != nil {
		env[3].Value = tls.Mode
		if tls.CASecretRef != nil {
			volumes = append(volumes, corev1.Volume{Name: "tls-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: tls.CASecretRef.Name,
				Items:      []corev1.KeyToPath{{Key: tls.CASecretRef.Key, Path: "ca.crt"}},
			}}})
			mounts = append(mounts, corev1.VolumeMount{Name: "tls-ca", MountPath: TLSDir + "/ca", ReadOnly: true})
			env = append(env, corev1.EnvVar{Name: EnvSSLRootCert, Value: TLSDir + "/ca/ca.crt"})
		}
		if tls.ClientCertSecretRef != nil {
			volumes = append(volumes, corev1.Volume{Name: "tls-client", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  tls.ClientCertSecretRef.Name,
				DefaultMode: ptr.To[int32](0o440),
			}}})
			mounts = append(mounts, corev1.VolumeMount{Name: "tls-client", MountPath: TLSDir + "/client", ReadOnly: true})
			env = append(env,
				corev1.EnvVar{Name: EnvSSLCert, Value: TLSDir + "/client/tls.crt"},
				corev1.EnvVar{Name: EnvSSLKey, Value: TLSDir + "/client/tls.key"})
		}
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: Name(s), Namespace: s.Owner.GetNamespace(), Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](2),
			ActiveDeadlineSeconds:   ptr.To[int64](300),
			TTLSecondsAfterFinished: ptr.To[int32](3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						RunAsUser:      ptr.To[int64](nonRootUID),
						FSGroup:        ptr.To[int64](nonRootUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Volumes: volumes,
					Containers: []corev1.Container{{
						Name:         "job",
						Image:        s.Image,
						Command:      []string{"/manager"},
						Args:         append([]string{"job", s.Command}, s.Args...),
						Env:          env,
						VolumeMounts: mounts,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
						},
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(s.Owner, job, scheme); err != nil {
		return nil, err
	}
	return job, nil
}
