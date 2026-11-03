package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/ericogr/agent-sandbox-poc/internal/environments"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Erro:", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 {
		return errors.New("uso: sandboxctl {create|list|stop|start|open|password|delete} [flags] [nomes]")
	}
	command := args[0]
	switch command {
	case "create", "list", "stop", "start", "open", "password", "delete":
	case "help", "-h", "--help":
		fmt.Fprintln(out, "sandboxctl create [--ttl 2h] [--parallel 1] [--timeout 3m] [--root .] nome...\nsandboxctl list [--root .]\nsandboxctl stop [--timeout 3m] [--root .] nome...\nsandboxctl start [--ttl 2h] [--timeout 3m] [--root .] nome...\nsandboxctl open [--port 8080] [--root .] nome\nsandboxctl password [--root .] nome\nsandboxctl delete [--purge] [--timeout 3m] [--root .] nome...\nFlags antes dos nomes. --root aponta para o repositório com .state.\n--ttl: prazo desde create/start; sem flag, create não expira. Repetir create preserva modo/prazo.\nstart sem --ttl limpa prazo ao retomar; se já ativo, preserva prazo vigente.\nstop/expiração encerram processos e sessões de depuração; PVC e Secret são preservados.")
		return nil
	default:
		return fmt.Errorf("comando %q desconhecido", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(errOut)
	root := flags.String("root", ".", "diretório do repositório com .state")
	timeout := flags.Duration("timeout", 3*time.Minute, "limite para provisionar/remover cada ambiente e conectar")
	parallel := 1
	port := 8080
	purge := false
	var ttl time.Duration
	if command == "create" || command == "start" {
		flags.DurationVar(&ttl, "ttl", 0, "expiração após duração positiva (ex.: 30m, 2h); preserva dados")
	}
	if command == "create" {
		flags.IntVar(&parallel, "parallel", 1, "criações simultâneas (1–8)")
	}
	if command == "open" {
		flags.IntVar(&port, "port", 8080, "porta em 127.0.0.1")
	}
	if command == "delete" {
		flags.BoolVar(&purge, "purge", false, "apaga também PVC e Secret próprios, destruindo os dados")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	names := flags.Args()
	ttlExplicit := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "ttl" {
			ttlExplicit = true
		}
	})
	if ttlExplicit && ttl <= 0 {
		return errors.New("ttl deve ser positivo")
	}
	if *timeout <= 0 {
		return errors.New("timeout deve ser positivo")
	}
	if parallel < 1 || parallel > 8 {
		return errors.New("parallel deve estar entre 1 e 8")
	}
	if port < 1 || port > 65535 {
		return errors.New("porta deve estar entre 1 e 65535")
	}
	if command == "list" && len(names) != 0 {
		return errors.New("list não recebe nomes")
	}
	if command != "list" && len(names) == 0 {
		return errors.New("informe ao menos um nome")
	}
	if (command == "open" || command == "password") && len(names) != 1 {
		return errors.New("comando recebe exatamente um nome")
	}
	seen := map[string]bool{}
	for _, name := range names {
		if err := environments.ValidateName(name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("nome repetido: %s", name)
		}
		seen[name] = true
	}
	connectCtx, cancel := context.WithTimeout(ctx, *timeout)
	m, cfg, err := environments.Connect(connectCtx, *root)
	cancel()
	if err != nil {
		return err
	}
	if command == "create" {
		image, err := os.ReadFile(filepath.Join(*root, ".state", "dev-image"))
		if err != nil {
			return fmt.Errorf("imagem não registrada: execute make dev-up primeiro: %w", err)
		}
		m.Image = strings.TrimSpace(string(image))
		if !strings.HasPrefix(m.Image, "agent-sandbox-go-dev:") || strings.ContainsAny(m.Image, " \t\r\n") {
			return errors.New("registro de imagem inválido; execute make dev-up")
		}
	}
	switch command {
	case "list":
		c, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		entries, err := m.List(c)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NOME\tESTADO\tEXPIRA (UTC)\tMOTIVO")
		for _, e := range entries {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Name, e.Status, e.Expires, e.Reason)
		}
		return w.Flush()
	case "password":
		c, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		password, err := m.Password(c, names[0])
		if err != nil {
			return err
		}
		fmt.Fprintln(out, password)
		return nil
	case "open":
		return m.Open(ctx, cfg, names[0], port, *timeout, out, errOut)
	default:
		errs := environments.RunParallel(ctx, names, parallel, func(ctx context.Context, name string) error {
			c, cancel := context.WithTimeout(ctx, *timeout)
			defer cancel()
			if command == "create" {
				return m.CreateWithTTL(c, name, ttl)
			}
			if command == "stop" {
				return m.Stop(c, name)
			}
			if command == "start" {
				return m.Start(c, name, ttl)
			}
			return m.Delete(c, name, purge)
		})
		failed := false
		for i, err := range errs {
			if err != nil {
				failed = true
				fmt.Fprintf(errOut, "%s: %v\n", names[i], err)
			} else {
				message := "operação concluída; consulte list para o estado"
				if command == "start" {
					message = "pronto; processos e depuração devem ser reabertos"
				}
				if command == "stop" {
					message = "suspenso; processos encerrados, PVC e Secret preservados"
				}
				if command == "delete" {
					message = "removido; PVC e Secret preservados"
					if purge {
						message = "removido com PVC e Secret"
					}
				}
				fmt.Fprintf(out, "%s: %s\n", names[i], message)
			}
		}
		if failed {
			return errors.New("um ou mais ambientes falharam")
		}
		return nil
	}
}
