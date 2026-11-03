package environments

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	sandboxv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

const trackingLabel = "agents.x-k8s.io/sandbox-name-hash"

func controlledBy(obj, sb metav1.Object) bool {
	if obj == nil || sb == nil || obj.GetNamespace() != sb.GetNamespace() {
		return false
	}
	owner := metav1.GetControllerOf(obj)
	return owner != nil && owner.APIVersion == sandboxv1.GroupVersion.String() && owner.Kind == "Sandbox" && owner.Name == sb.GetName() && owner.UID == sb.GetUID()
}

func sameConfiguration(existing, desired sandboxv1.SandboxSpec) bool {
	existing.OperatingMode, desired.OperatingMode = "", ""
	existing.ShutdownTime, desired.ShutdownTime = nil, nil
	// The CLI migrates the original service=false blueprint separately after ownership checks.
	existing.Service = desired.Service
	return equality.Semantic.DeepEqual(existing, desired)
}

func expired(sb *sandboxv1.Sandbox, now time.Time) bool {
	return sb.Spec.ShutdownTime != nil && !now.Before(sb.Spec.ShutdownTime.Time)
}

func (m *Manager) getSandbox(ctx context.Context, name string) (*sandboxv1.Sandbox, error) {
	sb, err := m.Sandboxes.AgentsV1beta1().Sandboxes(Namespace).Get(ctx, base(name), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err = m.owned(sb, name); err != nil {
		return nil, err
	}
	return sb, nil
}

func (m *Manager) automaticService(ctx context.Context, name string, sb metav1.Object) (*corev1.Service, error) {
	svc, err := m.Core.CoreV1().Services(Namespace).Get(ctx, base(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(base(name)))
	expectedSelector := map[string]string{trackingLabel: fmt.Sprintf("%08x", h.Sum32())}
	if !controlledBy(svc, sb) || svc.Spec.ClusterIP != corev1.ClusterIPNone || !reflect.DeepEqual(svc.Spec.Selector, expectedSelector) {
		return nil, fmt.Errorf("Service %s não pertence ao Sandbox esperado ou é incompatível; operação recusada", svc.Name)
	}
	return svc, nil
}

// updateSandbox preserves unrelated fields and rejects replacement during a retry.
func (m *Manager) updateSandbox(ctx context.Context, name string, change func(*sandboxv1.Sandbox) error) error {
	original, err := m.getSandbox(ctx, name)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sb, err := m.getSandbox(ctx, name)
		if err != nil {
			return err
		}
		if sb.UID != original.UID {
			return errors.New("Sandbox foi substituído; operação interrompida")
		}
		before := sb.Spec.DeepCopy()
		annotations := sb.DeepCopy().Annotations
		if err = change(sb); err != nil {
			return err
		}
		if equality.Semantic.DeepEqual(*before, sb.Spec) && reflect.DeepEqual(annotations, sb.Annotations) {
			return nil
		}
		_, err = m.Sandboxes.AgentsV1beta1().Sandboxes(Namespace).Update(ctx, sb, metav1.UpdateOptions{})
		return err
	})
}

func (m *Manager) enableService(ctx context.Context, name string) error {
	return m.updateSandbox(ctx, name, func(sb *sandboxv1.Sandbox) error {
		if sb.Spec.Service != nil && *sb.Spec.Service {
			return nil
		}
		desired, err := m.serviceMigration(sb)
		if err != nil {
			return err
		}
		enabled := true
		sb.Spec.Service = &enabled
		sb.Annotations[SpecAnnotation] = desired.Annotations[SpecAnnotation]
		return nil
	})
}

func (m *Manager) removeLegacyService(ctx context.Context, name string) error {
	legacy, err := m.Core.CoreV1().Services(Namespace).Get(ctx, base(name)+"-ide", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sb, err := m.getSandbox(ctx, name)
	if err != nil {
		return err
	}
	svc, err := m.automaticService(ctx, name, sb)
	if err != nil {
		return err
	}
	if svc == nil {
		return errors.New("Service automático ainda ausente; Service legado preservado")
	}
	if err = m.owned(legacy, name); err != nil {
		return err
	}
	_, desiredLegacy, _, err := m.desired(name)
	if err != nil {
		return err
	}
	if err = m.compatible(legacy, desiredLegacy); err != nil {
		return err
	}
	uid, rv := legacy.UID, legacy.ResourceVersion
	err = m.Core.CoreV1().Services(Namespace).Delete(ctx, legacy.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (m *Manager) Stop(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	rs, err := m.resources(name)
	if err != nil {
		return err
	}
	if _, err = m.inspect(ctx, name, rs, false); err != nil {
		return err
	}
	if err = m.updateSandbox(ctx, name, func(sb *sandboxv1.Sandbox) error {
		sb.Spec.OperatingMode = sandboxv1.SandboxOperatingModeSuspended
		return nil
	}); err != nil {
		return err
	}
	err = wait.PollUntilContextCancel(ctx, m.poll(), true, func(ctx context.Context) (bool, error) {
		sb, err := m.getSandbox(ctx, name)
		if err != nil {
			return false, err
		}
		pod, err := m.getPod(ctx, name, sb)
		if err != nil || pod != nil {
			return false, err
		}
		// Expiry takes precedence over suspension in the controller.
		for _, cond := range sb.Status.Conditions {
			if cond.ObservedGeneration < sb.Generation {
				continue
			}
			if cond.Type == "Suspended" && cond.Status == metav1.ConditionTrue {
				return true, nil
			}
			if expired(sb, time.Now()) && cond.Type == "Ready" && cond.Reason == sandboxv1.SandboxReasonExpired {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("aguardar suspensão %s: %w (dados preservados)", name, err)
	}
	return nil
}

func (m *Manager) Start(ctx context.Context, name string, ttl time.Duration) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if ttl < 0 {
		return errors.New("ttl deve ser positivo")
	}
	rs, err := m.resources(name)
	if err != nil {
		return err
	}
	found, err := m.inspect(ctx, name, rs, false)
	if err != nil {
		return err
	}
	if found[0] == nil || found[1] == nil || found[2] == nil {
		return fmt.Errorf("ambiente %s incompleto; execute create primeiro", name)
	}
	if found[3] != nil {
		if err = m.compatible(found[3], rs[3].desired); err != nil {
			return err
		}
	}
	if err = m.enableService(ctx, name); err != nil {
		return err
	}
	now := time.Now()
	if err = m.updateSandbox(ctx, name, func(sb *sandboxv1.Sandbox) error {
		if ttl > 0 {
			deadline := metav1.NewTime(now.Add(ttl))
			sb.Spec.ShutdownTime = &deadline
		} else if sb.Spec.OperatingMode == sandboxv1.SandboxOperatingModeSuspended || expired(sb, now) {
			sb.Spec.ShutdownTime = nil
		}
		policy := sandboxv1.ShutdownPolicyRetain
		sb.Spec.ShutdownPolicy = &policy
		sb.Spec.OperatingMode = sandboxv1.SandboxOperatingModeRunning
		return nil
	}); err != nil {
		return err
	}
	if err = m.WaitReady(ctx, name); err != nil {
		return err
	}
	return m.removeLegacyService(ctx, name)
}

type Entry struct{ Name, Status, Reason, Expires string }

func (m *Manager) List(ctx context.Context) ([]Entry, error) {
	list, err := m.Sandboxes.AgentsV1beta1().Sandboxes(Namespace).List(ctx, metav1.ListOptions{LabelSelector: ManagerLabel + "=" + manager})
	if err != nil {
		return nil, err
	}
	result := []Entry{}
	now := time.Now()
	for _, sb := range list.Items {
		name := sb.Labels[EnvironmentLabel]
		if ValidateName(name) != nil || sb.Name != base(name) || m.owned(&sb, name) != nil {
			continue
		}
		entry := Entry{Name: name, Status: "Pending", Expires: "-"}
		if sb.Spec.ShutdownTime != nil {
			entry.Expires = sb.Spec.ShutdownTime.UTC().Format(time.RFC3339)
		}
		for _, cond := range sb.Status.Conditions {
			if cond.ObservedGeneration < sb.Generation {
				continue
			}
			if cond.Type == "Ready" {
				entry.Reason = cond.Reason
				if cond.Status == metav1.ConditionTrue {
					entry.Status = "Running"
				}
				if cond.Reason == sandboxv1.SandboxReasonReconcilerError || cond.Reason == sandboxv1.SandboxReasonInvalidConfiguration || cond.Reason == sandboxv1.SandboxReasonMultiplePods {
					entry.Status = "Failed"
				}
			}
			if cond.Type == "Finished" && cond.Status == metav1.ConditionTrue {
				entry.Status = "Finished"
				entry.Reason = cond.Reason
			}
		}
		if expired(&sb, now) {
			entry.Status = "Expired"
		} else if sb.Spec.OperatingMode == sandboxv1.SandboxOperatingModeSuspended {
			entry.Status = "Suspending"
			for _, cond := range sb.Status.Conditions {
				if cond.Type == "Suspended" && cond.Status == metav1.ConditionTrue && cond.ObservedGeneration >= sb.Generation {
					entry.Status = "Suspended"
				}
			}
		}
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (m *Manager) serviceMigration(sb *sandboxv1.Sandbox) (*sandboxv1.Sandbox, error) {
	name := sb.Labels[EnvironmentLabel]
	// Only the CLI's exact legacy blueprint is eligible for automatic migration.
	expected := *m
	if len(sb.Spec.PodTemplate.Spec.Containers) != 1 {
		return nil, fmt.Errorf("Sandbox %s incompatível com migração do CLI", sb.Name)
	}
	expected.Image = sb.Spec.PodTemplate.Spec.Containers[0].Image
	desired, svc, pvc, err := expected.desired(name)
	if err != nil {
		return nil, err
	}
	legacy := desired.Spec.DeepCopy()
	disabled := false
	legacy.Service = &disabled
	fingerprint, err := specFingerprint(*legacy, svc.Spec, pvc.Spec)
	if err != nil {
		return nil, err
	}
	if sb.Spec.Service == nil || *sb.Spec.Service || sb.Annotations[SpecAnnotation] != fingerprint || !sameConfiguration(sb.Spec, desired.Spec) {
		return nil, fmt.Errorf("Sandbox %s não corresponde ao legado do CLI; migração recusada", sb.Name)
	}
	return desired, nil
}
