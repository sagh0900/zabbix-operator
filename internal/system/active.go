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

package system

import (
	"context"
	"net"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ActiveProbe reports whether a server pod is the active HA node.
type ActiveProbe func(ctx context.Context, pod *corev1.Pod) bool

// TCPActiveProbe checks whether the pod accepts connections on the trapper port: only the
// active HA node listens there, standby nodes refuse. The operator dials the pod IP
// directly, never the Service, which routes by the result of this check.
func TCPActiveProbe(ctx context.Context, pod *corev1.Pod) bool {
	if pod.Status.PodIP == "" {
		return false
	}
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(TrapperPort)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
