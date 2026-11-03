package environments

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/ericogr/agent-sandbox-poc/manifests"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	sandboxv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

const (
	Namespace         = "default"
	Context           = "kind-agent-sandbox-test"
	ManagerLabel      = "poc.agent-sandbox.local/manager"
	EnvironmentLabel  = "poc.agent-sandbox.local/environment"
	ClusterAnnotation = "poc.agent-sandbox.local/cluster-uid"
	SpecAnnotation    = "poc.agent-sandbox.local/cli-spec"
	manager           = "env-cli"
)

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,35}[a-z0-9]$|^[a-z]$`)

func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("nome %q inválido: use 1–37 caracteres minúsculos, números e hífen, iniciando por letra", name)
	}
	return nil
}
func base(name string) string { return "env-" + name }
func (m *Manager) metadata(name, resource string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: resource, Namespace: Namespace, Labels: map[string]string{ManagerLabel: manager, EnvironmentLabel: name}, Annotations: map[string]string{ClusterAnnotation: m.ClusterUID}}
}
func (m *Manager) owned(obj metav1.Object, name string) error {
	if obj.GetLabels()[ManagerLabel] != manager || obj.GetLabels()[EnvironmentLabel] != name || obj.GetAnnotations()[ClusterAnnotation] != m.ClusterUID || len(obj.GetOwnerReferences()) != 0 {
		return fmt.Errorf("recurso %s não pertence ao CLI/ambiente %s; operação recusada", obj.GetName(), name)
	}
	if obj.GetDeletionTimestamp() != nil {
		return fmt.Errorf("recurso %s está sendo removido; aguarde", obj.GetName())
	}
	return nil
}
func (m *Manager) desired(name string) (*sandboxv1.Sandbox, *corev1.Service, *corev1.PersistentVolumeClaim, error) {
	sb := &sandboxv1.Sandbox{}
	svc := &corev1.Service{}
	pvc := &corev1.PersistentVolumeClaim{}
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifests.Dev), 4096)
	if err := dec.Decode(sb); err != nil {
		return nil, nil, nil, err
	}
	if err := dec.Decode(svc); err != nil {
		return nil, nil, nil, err
	}
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifests.Storage), 4096).Decode(pvc); err != nil {
		return nil, nil, nil, err
	}
	sb.ObjectMeta = m.metadata(name, base(name))
	svc.ObjectMeta = m.metadata(name, base(name)+"-ide")
	pvc.ObjectMeta = m.metadata(name, base(name)+"-home")
	sb.Spec.PodTemplate.ObjectMeta.Labels = map[string]string{ManagerLabel: manager, EnvironmentLabel: name}
	enabled := true
	sb.Spec.Service = &enabled
	sb.Spec.OperatingMode = sandboxv1.SandboxOperatingModeRunning
	sb.Spec.FailedPodPolicy = sandboxv1.FailedPodPolicyRetain
	policy := sandboxv1.ShutdownPolicyRetain
	sb.Spec.ShutdownPolicy = &policy
	sb.Spec.PodTemplate.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolTCP
	sb.Spec.PodTemplate.Spec.Containers[0].Image = m.Image
	sb.Spec.PodTemplate.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name = base(name) + "-auth"
	sb.Spec.PodTemplate.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = pvc.Name
	svc.Spec.Selector = map[string]string{ManagerLabel: manager, EnvironmentLabel: name}
	svc.Spec.Ports[0].Protocol = corev1.ProtocolTCP
	fingerprint, err := specFingerprint(sb.Spec, svc.Spec, pvc.Spec)
	if err != nil {
		return nil, nil, nil, err
	}
	sb.Annotations[SpecAnnotation] = fingerprint
	return sb, svc, pvc, nil
}

func specFingerprint(sb sandboxv1.SandboxSpec, svc corev1.ServiceSpec, pvc corev1.PersistentVolumeClaimSpec) (string, error) {
	data, err := json.Marshal(struct {
		Sandbox sandboxv1.SandboxSpec
		Service corev1.ServiceSpec
		PVC     corev1.PersistentVolumeClaimSpec
	}{sb, svc, pvc})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}
