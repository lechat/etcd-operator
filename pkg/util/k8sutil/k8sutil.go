// Copyright 2016 The etcd-operator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8sutil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/etcd-operator/pkg/spec"
	"github.com/coreos/etcd-operator/pkg/util"
	"github.com/coreos/etcd-operator/pkg/util/constants"
	"github.com/coreos/etcd-operator/pkg/util/etcdutil"
	"github.com/coreos/etcd-operator/pkg/util/retryutil"

	"k8s.io/kubernetes/pkg/api"
	apierrors "k8s.io/kubernetes/pkg/api/errors"
	unversionedAPI "k8s.io/kubernetes/pkg/api/unversioned"
	k8sv1api "k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/client/restclient"
	"k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/util/intstr"
	"k8s.io/kubernetes/pkg/watch"
)

const (
	// TODO: This is constant for current purpose. We might make it configurable later.
	etcdDir                    = "/var/etcd"
	dataDir                    = etcdDir + "/data"
	backupFile                 = "/var/etcd/latest.backup"
	etcdVersionAnnotationKey   = "etcd.version"
	annotationPrometheusScrape = "prometheus.io/scrape"
	annotationPrometheusPort   = "prometheus.io/port"
)

func GetEtcdVersion(pod *api.Pod) string {
	return pod.Annotations[etcdVersionAnnotationKey]
}

func SetEtcdVersion(pod *api.Pod, version string) {
	pod.Annotations[etcdVersionAnnotationKey] = version
}

func GetPodNames(pods []*api.Pod) []string {
	res := []string{}
	for _, p := range pods {
		res = append(res, p.Name)
	}
	return res
}

func makeRestoreInitContainerSpec(backupAddr, name, token, version string) string {
	spec := []api.Container{
		{
			Name:  "fetch-backup",
			Image: "tutum/curl",
			Command: []string{
				"/bin/sh", "-c",
				fmt.Sprintf("curl -o %s %s", backupFile, util.MakeBackupURL(backupAddr, version)),
			},
			VolumeMounts: []api.VolumeMount{
				{Name: "etcd-data", MountPath: etcdDir},
			},
		},
		{
			Name:  "restore-datadir",
			Image: MakeEtcdImage(version),
			Command: []string{
				"/bin/sh", "-c",
				fmt.Sprintf("ETCDCTL_API=3 etcdctl snapshot restore %[1]s"+
					" --name %[2]s"+
					" --initial-cluster %[2]s=http://%[2]s:2380"+
					" --initial-cluster-token %[3]s"+
					" --initial-advertise-peer-urls http://%[2]s:2380"+
					" --data-dir %[4]s", backupFile, name, token, dataDir),
			},
			VolumeMounts: []api.VolumeMount{
				{Name: "etcd-data", MountPath: etcdDir},
			},
		},
	}
	b, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func MakeEtcdImage(version string) string {
	return fmt.Sprintf("quay.io/coreos/etcd:%v", version)
}

func GetNodePortString(srv *api.Service) string {
	return fmt.Sprint(srv.Spec.Ports[0].NodePort)
}

func MakeBackupHostPort(clusterName string) string {
	return fmt.Sprintf("%s:%d", MakeBackupName(clusterName), constants.DefaultBackupPodHTTPPort)
}

func PodWithAddMemberInitContainer(p *api.Pod, endpoints []string, name string, peerURLs []string, cs *spec.ClusterSpec) *api.Pod {
	containerSpec := []api.Container{
		{
			Name:  "add-member",
			Image: MakeEtcdImage(cs.Version),
			Command: []string{
				"/bin/sh", "-c",
				fmt.Sprintf("ETCDCTL_API=3 etcdctl --endpoints=%s member add %s --peer-urls=%s", strings.Join(endpoints, ","), name, strings.Join(peerURLs, ",")),
			},
			Env: []api.EnvVar{envPodIP},
		},
	}
	b, err := json.Marshal(containerSpec)
	if err != nil {
		panic(err)
	}
	p.Annotations[k8sv1api.PodInitContainersAnnotationKey] = string(b)
	return p
}

func PodWithNodeSelector(p *api.Pod, ns map[string]string) *api.Pod {
	p.Spec.NodeSelector = ns
	return p
}

func MakeBackupName(clusterName string) string {
	return fmt.Sprintf("%s-backup-tool", clusterName)
}

func CreateEtcdMemberService(kclient *unversioned.Client, etcdName, clusterName, ns string) (*api.Service, error) {
	svc := makeEtcdMemberService(etcdName, clusterName)
	retSvc, err := kclient.Services(ns).Create(svc)
	if err != nil {
		return nil, err
	}
	return retSvc, nil
}

func CreateEtcdService(kclient *unversioned.Client, clusterName, ns string) (*api.Service, error) {
	svc := makeEtcdService(clusterName)
	retSvc, err := kclient.Services(ns).Create(svc)
	if err != nil {
		return nil, err
	}
	return retSvc, nil
}

func CreateAndWaitPod(kclient *unversioned.Client, ns string, pod *api.Pod, timeout time.Duration) (*api.Pod, error) {
	if _, err := kclient.Pods(ns).Create(pod); err != nil {
		return nil, err
	}
	// TODO: cleanup pod on failure
	w, err := kclient.Pods(ns).Watch(api.SingleObject(api.ObjectMeta{Name: pod.Name}))
	if err != nil {
		return nil, err
	}
	_, err = watch.Until(timeout, w, unversioned.PodRunning)

	pod, err = kclient.Pods(ns).Get(pod.Name)
	if err != nil {
		return nil, err
	}

	return pod, nil
}

func makeEtcdService(clusterName string) *api.Service {
	labels := map[string]string{
		"app":          "etcd",
		"etcd_cluster": clusterName,
	}
	svc := &api.Service{
		ObjectMeta: api.ObjectMeta{
			Name:   clusterName,
			Labels: labels,
		},
		Spec: api.ServiceSpec{
			Ports: []api.ServicePort{
				{
					Name:       "client",
					Port:       2379,
					TargetPort: intstr.FromInt(2379),
					Protocol:   api.ProtocolTCP,
				},
			},
			Selector: labels,
		},
	}
	return svc
}

// TODO: converge the port logic with member ClientAddr() and PeerAddr()
func makeEtcdMemberService(etcdName, clusterName string) *api.Service {
	labels := map[string]string{
		"app":          "etcd",
		"etcd_node":    etcdName,
		"etcd_cluster": clusterName,
	}
	svc := &api.Service{
		ObjectMeta: api.ObjectMeta{
			Name:   etcdName,
			Labels: labels,
			Annotations: map[string]string{
				annotationPrometheusScrape: "true",
				annotationPrometheusPort:   "2379",
			},
		},
		Spec: api.ServiceSpec{
			Ports: []api.ServicePort{
				{
					Name:       "server",
					Port:       2380,
					TargetPort: intstr.FromInt(2380),
					Protocol:   api.ProtocolTCP,
				},
				{
					Name:       "client",
					Port:       2379,
					TargetPort: intstr.FromInt(2379),
					Protocol:   api.ProtocolTCP,
				},
			},
			Selector: labels,
		},
	}
	return svc
}

func AddRecoveryToPod(pod *api.Pod, clusterName, name, token string, cs *spec.ClusterSpec) {
	pod.Annotations[k8sv1api.PodInitContainersAnnotationKey] =
		makeRestoreInitContainerSpec(MakeBackupHostPort(clusterName), name, token, cs.Version)
}

func MakeEtcdPod(m *etcdutil.Member, initialCluster []string, clusterName, state, token string, cs *spec.ClusterSpec) *api.Pod {
	commands := fmt.Sprintf("/usr/local/bin/etcd --data-dir=%s --name=%s --initial-advertise-peer-urls=%s "+
		"--listen-peer-urls=http://0.0.0.0:2380 --listen-client-urls=http://0.0.0.0:2379 --advertise-client-urls=%s "+
		"--initial-cluster=%s --initial-cluster-state=%s",
		dataDir, m.Name, m.PeerAddr(), m.ClientAddr(), strings.Join(initialCluster, ","), state)
	if state == "new" {
		commands = fmt.Sprintf("%s --initial-cluster-token=%s", commands, token)
	}
	container := containerWithLivenessProbe(etcdContainer(commands, cs.Version), etcdLivenessProbe())
	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			Name: m.Name,
			Labels: map[string]string{
				"app":          "etcd",
				"etcd_node":    m.Name,
				"etcd_cluster": clusterName,
			},
			Annotations: map[string]string{},
		},
		Spec: api.PodSpec{
			Containers:    []api.Container{container},
			RestartPolicy: api.RestartPolicyNever,
			Volumes: []api.Volume{
				{Name: "etcd-data", VolumeSource: api.VolumeSource{EmptyDir: &api.EmptyDirVolumeSource{}}},
			},
		},
	}

	SetEtcdVersion(pod, cs.Version)

	if cs.AntiAffinity {
		pod = PodWithAntiAffinity(pod, clusterName)
	}

	if len(cs.NodeSelector) != 0 {
		pod = PodWithNodeSelector(pod, cs.NodeSelector)
	}

	return pod
}

func MustGetInClusterMasterHost() string {
	cfg, err := restclient.InClusterConfig()
	if err != nil {
		panic(err)
	}
	return cfg.Host
}

// tlsConfig isn't modified inside this function.
// The reason it's a pointer is that it's not necessary to have tlsconfig to create a client.
func MustCreateClient(host string, tlsInsecure bool, tlsConfig *restclient.TLSClientConfig) *unversioned.Client {
	if len(host) == 0 {
		c, err := unversioned.NewInCluster()
		if err != nil {
			panic(err)
		}
		return c
	}
	cfg := &restclient.Config{
		Host:  host,
		QPS:   100,
		Burst: 100,
	}
	hostUrl, err := url.Parse(host)
	if err != nil {
		panic(fmt.Sprintf("error parsing host url %s : %v", host, err))
	}
	if hostUrl.Scheme == "https" {
		cfg.TLSClientConfig = *tlsConfig
		cfg.Insecure = tlsInsecure
	}
	c, err := unversioned.New(cfg)
	if err != nil {
		panic(err)
	}
	return c
}

func IsKubernetesResourceAlreadyExistError(err error) bool {
	se, ok := err.(*apierrors.StatusError)
	if !ok {
		return false
	}
	if se.Status().Code == http.StatusConflict && se.Status().Reason == unversionedAPI.StatusReasonAlreadyExists {
		return true
	}
	return false
}

func IsKubernetesResourceNotFoundError(err error) bool {
	se, ok := err.(*apierrors.StatusError)
	if !ok {
		return false
	}
	if se.Status().Code == http.StatusNotFound && se.Status().Reason == unversionedAPI.StatusReasonNotFound {
		return true
	}
	return false
}

func ListETCDCluster(host, ns string, httpClient *http.Client) (*http.Response, error) {
	return httpClient.Get(fmt.Sprintf("%s/apis/coreos.com/v1/namespaces/%s/etcdclusters",
		host, ns))
}

func WatchETCDCluster(host, ns string, httpClient *http.Client, resourceVersion string) (*http.Response, error) {
	return httpClient.Get(fmt.Sprintf("%s/apis/coreos.com/v1/namespaces/%s/etcdclusters?watch=true&resourceVersion=%s",
		host, ns, resourceVersion))
}

func WaitEtcdTPRReady(httpClient *http.Client, interval, timeout time.Duration, host, ns string) error {
	return retryutil.Retry(interval, int(timeout/interval), func() (bool, error) {
		resp, err := ListETCDCluster(host, ns, httpClient)
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			return true, nil
		case http.StatusNotFound: // not set up yet. wait.
			return false, nil
		default:
			return false, fmt.Errorf("invalid status code: %v", resp.Status)
		}
	})
}

func EtcdPodListOpt(clusterName string) api.ListOptions {
	return api.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			"etcd_cluster": clusterName,
			"app":          "etcd",
		}),
	}
}
