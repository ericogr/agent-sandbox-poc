package environments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	sandboxv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxclient "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned"
)

type Manager struct {
	Core              kubernetes.Interface
	Sandboxes         sandboxclient.Interface
	ClusterUID, Image string
	PollInterval      time.Duration
}
type resource struct {
	kind    string
	desired metav1.Object
	get     func(context.Context) (metav1.Object, error)
	create  func(context.Context) (metav1.Object, error)
	delete  func(context.Context, metav1.DeleteOptions) error
}

func (m *Manager) resources(name string) ([]resource, error) {
	sb, svc, pvc, err := m.desired(name)
	if err != nil {
		return nil, err
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	secret := &corev1.Secret{ObjectMeta: m.metadata(name, base(name)+"-auth"), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"password": []byte(hex.EncodeToString(random))}}
	sec := m.Core.CoreV1().Secrets(Namespace)
	pvcs := m.Core.CoreV1().PersistentVolumeClaims(Namespace)
	sandboxes := m.Sandboxes.AgentsV1beta1().Sandboxes(Namespace)
	services := m.Core.CoreV1().Services(Namespace)
	return []resource{
		{"Secret", secret, func(c context.Context) (metav1.Object, error) { return sec.Get(c, secret.Name, metav1.GetOptions{}) }, func(c context.Context) (metav1.Object, error) { return sec.Create(c, secret, metav1.CreateOptions{}) }, func(c context.Context, o metav1.DeleteOptions) error { return sec.Delete(c, secret.Name, o) }},
		{"PVC", pvc, func(c context.Context) (metav1.Object, error) { return pvcs.Get(c, pvc.Name, metav1.GetOptions{}) }, func(c context.Context) (metav1.Object, error) { return pvcs.Create(c, pvc, metav1.CreateOptions{}) }, func(c context.Context, o metav1.DeleteOptions) error { return pvcs.Delete(c, pvc.Name, o) }},
		{"Sandbox", sb, func(c context.Context) (metav1.Object, error) { return sandboxes.Get(c, sb.Name, metav1.GetOptions{}) }, func(c context.Context) (metav1.Object, error) { return sandboxes.Create(c, sb, metav1.CreateOptions{}) }, func(c context.Context, o metav1.DeleteOptions) error { return sandboxes.Delete(c, sb.Name, o) }},
		{"Service", svc, func(c context.Context) (metav1.Object, error) { return services.Get(c, svc.Name, metav1.GetOptions{}) }, nil, func(c context.Context, o metav1.DeleteOptions) error { return services.Delete(c, svc.Name, o) }},
	}, nil
}
func (m *Manager) compatible(obj, want metav1.Object) error {
	switch existing := obj.(type) {
	case *corev1.Secret:
		if len(existing.Data["password"]) == 0 {
			return fmt.Errorf("Secret %s sem senha; preservado para diagnóstico", existing.Name)
		}
	case *sandboxv1.Sandbox:
		if existing.Spec.Service == nil || !*existing.Spec.Service {
			if _, err := m.serviceMigration(existing); err != nil {
				return err
			}
		}
		if !sameConfiguration(existing.Spec, want.(*sandboxv1.Sandbox).Spec) {
			return fmt.Errorf("spec/imagem mudou em %s; execute delete e create para aplicar, preservando os dados", existing.Name)
		}
	case *corev1.Service:
		desired := want.(*corev1.Service)
		if existing.Spec.Type != desired.Spec.Type || !reflect.DeepEqual(existing.Spec.Selector, desired.Spec.Selector) || !reflect.DeepEqual(existing.Spec.Ports, desired.Spec.Ports) || len(existing.Spec.ExternalIPs) > 0 || existing.Spec.ExternalName != "" {
			return fmt.Errorf("Service %s incompatível", existing.Name)
		}
	case *corev1.PersistentVolumeClaim:
		desired := want.(*corev1.PersistentVolumeClaim)
		if !reflect.DeepEqual(existing.Spec.AccessModes, desired.Spec.AccessModes) || existing.Spec.Resources.Requests.Storage().Cmp(*desired.Spec.Resources.Requests.Storage()) != 0 {
			return fmt.Errorf("PVC %s incompatível", existing.Name)
		}
	}
	return nil
}
func (m *Manager) getPod(ctx context.Context, name string, sb metav1.Object) (*corev1.Pod, error) {
	pod, err := m.Core.CoreV1().Pods(Namespace).Get(ctx, base(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if controlledBy(pod, sb) {
		return pod, nil
	}
	return nil, fmt.Errorf("Pod %s não pertence ao Sandbox esperado; operação recusada", pod.Name)
}
func (m *Manager) inspect(ctx context.Context, name string, rs []resource, compatible bool) ([]metav1.Object, error) {
	found := make([]metav1.Object, len(rs))
	for i, r := range rs {
		obj, err := r.get(ctx)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.kind, err)
		}
		if err = m.owned(obj, name); err != nil {
			return nil, err
		}
		if compatible {
			if err = m.compatible(obj, r.desired); err != nil {
				return nil, err
			}
		}
		found[i] = obj
	}
	if _, err := m.getPod(ctx, name, found[2]); err != nil {
		return nil, err
	}
	if _, err := m.automaticService(ctx, name, found[2]); err != nil {
		return nil, err
	}
	return found, nil
}
func (m *Manager) Create(ctx context.Context, name string) error {
	return m.CreateWithTTL(ctx, name, 0)
}

func (m *Manager) CreateWithTTL(ctx context.Context, name string, ttl time.Duration) error {
	if ttl < 0 {
		return errors.New("ttl deve ser positivo")
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	if m.Image == "" {
		return errors.New("imagem ausente; execute make dev-up primeiro")
	}
	rs, err := m.resources(name)
	if err != nil {
		return err
	}
	found, err := m.inspect(ctx, name, rs, true)
	if err != nil {
		return err
	}
	createdSandbox := false
	for i, r := range rs[:3] {
		if found[i] != nil {
			continue
		}
		if i == 2 && ttl > 0 {
			deadline := metav1.NewTime(time.Now().Add(ttl))
			rs[2].desired.(*sandboxv1.Sandbox).Spec.ShutdownTime = &deadline
		}
		obj, err := r.create(ctx)
		if i == 2 && err == nil {
			createdSandbox = true
		}
		if apierrors.IsAlreadyExists(err) {
			obj, err = r.get(ctx)
		}
		if err != nil {
			return fmt.Errorf("criar %s: %w (recursos parciais preservados; repita create)", r.kind, err)
		}
		if err = m.owned(obj, name); err != nil {
			return err
		}
		if err = m.compatible(obj, r.desired); err != nil {
			return err
		}
	}
	if err := m.enableService(ctx, name); err != nil {
		return err
	}
	sb, err := m.getSandbox(ctx, name)
	if err != nil {
		return err
	}
	if !createdSandbox && (sb.Spec.OperatingMode == sandboxv1.SandboxOperatingModeSuspended || expired(sb, time.Now())) {
		return nil
	}
	if err := m.WaitReady(ctx, name); err != nil {
		return err
	}
	return m.removeLegacyService(ctx, name)
}
func (m *Manager) poll() time.Duration {
	if m.PollInterval > 0 {
		return m.PollInterval
	}
	return time.Second
}
func (m *Manager) WaitReady(ctx context.Context, name string) error {
	err := wait.PollUntilContextCancel(ctx, m.poll(), true, func(ctx context.Context) (bool, error) {
		sb, err := m.Sandboxes.AgentsV1beta1().Sandboxes(Namespace).Get(ctx, base(name), metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if err = m.owned(sb, name); err != nil {
			return false, err
		}
		if expired(sb, time.Now()) {
			return false, fmt.Errorf("ambiente %s expirou; use start [--ttl duração] para retomar", name)
		}
		if sb.Spec.OperatingMode == sandboxv1.SandboxOperatingModeSuspended {
			return false, fmt.Errorf("ambiente %s suspenso; use start para retomar", name)
		}
		pod, err := m.getPod(ctx, name, sb)
		if err != nil {
			return false, err
		}
		if pod == nil || pod.DeletionTimestamp != nil {
			return false, nil
		}
		podReady := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				podReady = true
			}
		}
		if !podReady {
			return false, nil
		}
		for _, cond := range sb.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue && cond.ObservedGeneration >= sb.Generation {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("aguardar %s Ready: %w (recursos preservados)", name, err)
	}
	return nil
}
func (m *Manager) Delete(ctx context.Context, name string, purge bool) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	rs, err := m.resources(name)
	if err != nil {
		return err
	}
	found, err := m.inspect(ctx, name, rs, false)
	if err != nil {
		return err
	}
	order := []int{3, 2}
	if purge {
		order = append(order, 1, 0)
	}
	for _, i := range order {
		obj := found[i]
		if obj == nil {
			continue
		}
		uid := obj.GetUID()
		current, err := rs[i].get(ctx)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if current.GetUID() != uid {
			return fmt.Errorf("%s foi substituído; interrompido", rs[i].kind)
		}
		if err = m.owned(current, name); err != nil {
			return err
		}
		rv := current.GetResourceVersion()
		policy := metav1.DeletePropagationForeground
		err = rs[i].delete(ctx, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}, PropagationPolicy: &policy})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("remover %s: %w", rs[i].kind, err)
		}
		err = wait.PollUntilContextCancel(ctx, m.poll(), true, func(ctx context.Context) (bool, error) {
			current, err := rs[i].get(ctx)
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			if current.GetUID() != uid {
				return false, fmt.Errorf("%s foi substituído durante remoção; interrompido", rs[i].kind)
			}
			return false, nil
		})
		if err != nil {
			return fmt.Errorf("aguardar remoção %s: %w", rs[i].kind, err)
		}
	}
	return nil
}
func (m *Manager) Password(ctx context.Context, name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	secret, err := m.Core.CoreV1().Secrets(Namespace).Get(ctx, base(name)+"-auth", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if err = m.owned(secret, name); err != nil {
		return "", err
	}
	password := string(secret.Data["password"])
	if password == "" {
		return "", errors.New("Secret sem senha")
	}
	return password, nil
}

func RunParallel(ctx context.Context, names []string, parallel int, run func(context.Context, string) error) []error {
	errs := make([]error, len(names))
	if parallel < 1 || parallel > 8 {
		for i := range errs {
			errs[i] = errors.New("parallel deve estar entre 1 e 8")
		}
		return errs
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					errs[i] = ctx.Err()
				} else {
					errs[i] = run(ctx, names[i])
				}
			}
		}()
	}
	for i := range names {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return errs
}
