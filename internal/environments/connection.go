package environments

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	sandboxclient "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned"
)

// Connect only accepts the dedicated POC context, namespace and recorded cluster UID.
func Connect(ctx context.Context, root string) (*Manager, *rest.Config, error) {
	recorded, err := os.ReadFile(filepath.Join(root, ".state", "cluster-uid"))
	if err != nil {
		return nil, nil, fmt.Errorf("registro de propriedade do cluster: %w", err)
	}
	uid := strings.TrimSpace(string(recorded))
	if uid == "" {
		return nil, nil, fmt.Errorf("registro de propriedade vazio")
	}
	raw, err := clientcmd.LoadFromFile(filepath.Join(root, ".state", "kubeconfig"))
	if err != nil {
		return nil, nil, err
	}
	chosen, ok := raw.Contexts[Context]
	if !ok || (chosen.Namespace != "" && chosen.Namespace != Namespace) {
		return nil, nil, fmt.Errorf("contexto %s ausente ou namespace diferente de default", Context)
	}
	cfg, err := clientcmd.NewNonInteractiveClientConfig(*raw, Context, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, nil, err
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	ns, err := core.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	if string(ns.UID) != uid {
		return nil, nil, fmt.Errorf("UID do cluster diverge; operação recusada")
	}
	version, err := core.Discovery().RESTClient().Get().AbsPath("/version").DoRaw(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !supportedVersion(version) {
		return nil, nil, fmt.Errorf("cluster incompatível: esperado Kubernetes v1.36.x")
	}
	if _, err = core.CoreV1().Namespaces().Get(ctx, Namespace, metav1.GetOptions{}); err != nil {
		return nil, nil, err
	}
	sandboxes, err := sandboxclient.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return &Manager{Core: core, Sandboxes: sandboxes, ClusterUID: uid}, cfg, nil
}
func (m *Manager) Open(ctx context.Context, cfg *rest.Config, name string, port int, timeout time.Duration, out, errOut io.Writer) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("porta deve estar entre 1 e 65535")
	}
	initCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rs, err := m.resources(name)
	if err != nil {
		return err
	}
	found, err := m.inspect(initCtx, name, rs, false)
	if err != nil {
		return err
	}
	if found[2] == nil {
		return fmt.Errorf("ambiente %s não está criado", name)
	}
	if err = m.WaitReady(initCtx, name); err != nil {
		return err
	}
	pod, err := m.getPod(initCtx, name, found[2])
	if err != nil {
		return err
	}
	if pod == nil {
		return fmt.Errorf("Pod do ambiente %s ausente", name)
	}
	url := m.Core.CoreV1().RESTClient().Post().Resource("pods").Namespace(Namespace).Name(pod.Name).SubResource("portforward").URL()
	dialer := openingDialer{ctx: initCtx, config: cfg, url: url}
	stop := make(chan struct{})
	ready := make(chan struct{})
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			close(stop)
		case <-done:
		}
	}()
	forwarder, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("%d:8080", port)}, stop, ready, io.Discard, errOut)
	if err != nil {
		return err
	}
	go func() {
		select {
		case <-ready:
			fmt.Fprintf(out, "IDE: http://127.0.0.1:%d — senha: sandboxctl password %s. Ctrl+C encerra o túnel.\n", port, name)
		case <-done:
		}
	}()
	return forwarder.ForwardPorts()
}
