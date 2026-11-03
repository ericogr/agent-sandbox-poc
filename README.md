# Agent Sandbox: POC local com Go

Cluster kind dedicado com Agent Sandbox e IDE Go no navegador, acessada por port-forward local. O Make usa as ferramentas do `PATH`; não instala nem atualiza ferramentas no host. A IDE e suas ferramentas são instaladas somente na imagem customizada. A demo com SDK Go continua proposta separada.

## Ambiente e pré-requisitos

Verificado em 26/10/2026: Go 1.27.3, Docker cliente/daemon 29.8.2, kind 0.33.0 e kubectl 1.37.1 disponíveis. Já existem os clusters `hub` e `spoke1` (Kubernetes 1.32.2), com contexto ativo `kind-hub`. Use um cluster separado para esta POC.

O Make exige uma versão estável do kind **0.32.0 ou superior**; o kind 0.33.0 disponível atende ao requisito. Se estiver abaixo do mínimo, atualize apenas o kind manualmente. O kubectl 1.37.1 disponível é aceito e não precisa ser alterado. O Make aceita kubectl 1.35, 1.36 ou 1.37 para Kubernetes **1.36.1**, fixado pelo digest oficial `sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5` da [release kind v0.32.0](https://github.com/kubernetes-sigs/kind/releases/tag/v0.32.0). O cluster dedicado foi observado saudável com kind 0.33.0 e Kubernetes 1.36.1. O [skew suportado é de ±1 versão minor](https://kubernetes.io/releases/version-skew-policy/#kubectl); kubectl 1.37 não deve ser usado indiscriminadamente nos clusters existentes 1.32.2.

O SDK requer Go 1.26+; a release v1.0.8 declara toolchain Go 1.27.1, atendida pelo Go local. Também são necessários acesso aos registries, recursos locais para os pods, controller com extensões, router, template e warm pool. Consulte o [Go Client](https://agent-sandbox.sigs.k8s.io/docs/go-client/) e o [go.mod da release](https://github.com/kubernetes-sigs/agent-sandbox/blob/v1.0.8/go.mod).

## Operações com Make

Com kind estável 0.32.0 ou superior no `PATH`:

```sh
make help
make doctor
make up
make smoke
make status
make cleanup
```

`make up` executa sequencialmente `cluster-up`, `controller`, `router` e `pool`; também é possível executar cada target individualmente. O Make exige kind estável 0.32.0 ou superior e usa as ferramentas existentes (`make`, `sh`, `curl`, `sha256sum`, Docker e kubectl), sem instalar dependências. `doctor` verifica ferramentas, versões e acesso ao daemon Docker. A imagem Kubernetes 1.36 requer cgroup v2 no host.

O cluster tem nome fixo `agent-sandbox-test`, contexto `kind-agent-sandbox-test` e kubeconfig isolado em `.state/kubeconfig`, ignorado pelo Git. Todos os comandos kubectl recebem esse arquivo e contexto explicitamente. O kubeconfig padrão e os clusters `hub` e `spoke1` são preservados.

`cluster-up` recusa adotar um cluster existente sem registro de propriedade local. As operações seguintes e `cleanup` conferem o UID de `kube-system` contra `.state/cluster-uid`; divergência ou ausência interrompe a execução. `cleanup` remove somente o cluster dedicado após essa verificação. Preserve `.state` enquanto o cluster existir. Se a criação falhar antes de registrar o UID, investigue o cluster e o kubeconfig manualmente; o Make não remove nem adota automaticamente esse estado parcial. Uma operação interrompida abruptamente pode deixar `.state/lock`; remova esse diretório vazio apenas após confirmar que não há outra operação em execução.

Se o cluster gerenciado já existir em Kubernetes 1.35, as operações de provisionamento e `status` recusam reutilizá-lo. Após atualizar o kind, execute explicitamente `make cleanup` e depois `make up` para recriá-lo em 1.36.1. **O cleanup apaga os recursos da POC** e continua exigindo o registro local e UID correspondente; nenhuma recriação é automática. Apenas o cleanup dispensa a checagem da versão do servidor para permitir remover o cluster legado.

O controller core com extensões e o manifesto do router estão fixados em [v1.0.8](https://github.com/kubernetes-sigs/agent-sandbox/releases/tag/v1.0.8). Downloads HTTPS são verificados com SHA-256 fixado no script antes da aplicação; os hashes foram calculados dos arquivos oficiais dessa release. O router segue o [guia oficial](https://agent-sandbox.sigs.k8s.io/docs/getting_started/install_prerequisites/) com autenticação desabilitada **somente neste cluster local**, Service ClusterIP e sem portas públicas. As imagens auxiliares do router e runtime usam `latest-main`, como no guia, limitando a reprodutibilidade.

`pool` aplica `manifests/pool.yaml`, com template Python sem PVC e warm pool de uma réplica. `smoke` aplica um claim, aguarda Ready, exige nome de sandbox preenchido e remove o claim após sucesso. Em falha, preserva o claim para diagnóstico; repetir `make smoke` reaproveita o mesmo nome. O teste cobre provisionamento; execução de comandos e arquivos depende da demo Go abaixo. Para diagnóstico:

```sh
kubectl --kubeconfig .state/kubeconfig --context kind-agent-sandbox-test -n default describe sandboxclaim poc-smoke
kubectl --kubeconfig .state/kubeconfig --context kind-agent-sandbox-test -n default get events --sort-by=.metadata.creationTimestamp
```

## IDE Go no navegador

Com o cluster dedicado e controller prontos (`make up` na primeira utilização):

```sh
make dev-up
make dev-password
make dev-open
```

Abra **http://127.0.0.1:8080**, informe a senha mostrada por `dev-password` e use o terminal integrado para `git clone` em `/home/dev/project`. O workspace começa vazio; arquivos deste repositório no host não são copiados nem montados automaticamente. `dev-open` mantém o túnel em primeiro plano; Ctrl+C encerra apenas o túnel. A porta local 8080 precisa estar livre.

`dev-up` valida a propriedade do cluster, constrói a imagem, carrega-a no kind e aplica um `Sandbox` independente do pool Python, um Service ClusterIP e o PVC `go-dev-home` de 10 GiB. A imagem recebe uma tag derivada do seu ID; a primeira construção exige internet e pode levar alguns minutos; as seguintes reutilizam o cache. O estado do buildx fica em `.state/buildx`. Repetir `dev-up` sem mudanças preserva a IDE em execução. Se a imagem ou o manifesto mudar, o comando pede explicitamente `dev-down` seguido de `dev-up`, pois o controller não atualiza a spec do Pod existente. O manifesto segue o [exemplo oficial de VSCode Sandbox](https://agent-sandbox.sigs.k8s.io/docs/use-cases/examples/vscode-sandbox/), com imagem própria e PVC separado para preservar os dados na remoção do Sandbox.

A imagem fixa Go **1.27.3** e Git da base oficial `golang:1.27.3-bookworm` por digest, [code-server **4.143.1**](https://github.com/coder/code-server/releases/tag/v4.143.1), [gopls **0.24.0**](https://github.com/golang/tools/releases/tag/gopls%2Fv0.24.0), [Delve **1.27.2**](https://github.com/go-delve/delve/releases/tag/v1.27.2) e a extensão [Go **0.58.0**, publicada pelo namespace verificado `golang` no Open VSX](https://open-vsx.org/extension/golang/Go). Downloads do code-server e VSIX são verificados por SHA-256; ferramentas Go são compiladas durante o build com versões explícitas. Atualizações automáticas do editor/extensão e checagens de ferramentas Go ficam desativadas nas configurações iniciais. `GOTOOLCHAIN=local` evita download automático de outra versão do Go: projetos que exijam uma versão superior precisam de atualização deliberada da imagem. Arquiteturas de imagem suportadas: amd64 e arm64.

O processo executa com UID 1000, sem token da API Kubernetes e sem capabilities adicionais. A senha aleatória de 256 bits fica no Secret `go-dev-auth`, é reutilizada e só é impressa por `make dev-password`. O acesso usa autenticação por senha e túnel limitado a `127.0.0.1`; o Service não publica portas no host.

```sh
make dev-down  # remove somente Sandbox e Service da IDE
make dev-up    # reutiliza o mesmo projeto, configurações e senha
```

O PVC separado preserva **todo `/home/dev`**, incluindo projeto, configurações do editor, caches Go e arquivos pessoais. A extensão Go fica na imagem; extensões adicionais instaladas em `/opt/code-extensions` não persistem. Não há backup automático: **`make cleanup` remove o cluster kind e destrói também os dados locais do volume**. O volume não deve ser usado como única cópia do código. Esta POC usa o runtime padrão do kind; não configura gVisor/Kata, isolamento reforçado ou bloqueio de rede.

## Demo Go proposta

Após autorização para adicionar dependências, criar `go.mod` e `cmd/demo/main.go` usando o [quickstart Go oficial](https://agent-sandbox.sigs.k8s.io/docs/getting_started/go-sdk-quickstart/), fixando o SDK em `sigs.k8s.io/agent-sandbox/clients/go/sandbox@v1.0.8`. Executar com `KUBECONFIG="$PWD/.state/kubeconfig"` e contexto `kind-agent-sandbox-test`.

1. Criar sandbox de `python-sandbox-pool` em `default`, executar comando Python e conferir stdout/exit code.
2. Gravar `hello.txt`, ler o conteúdo e verificar existência; `Write` aceita nome simples, sem diretórios.
3. Criar um segundo sandbox, conferir que não contém o arquivo do primeiro e remover ambos com `DeleteAll`, usando contexto próprio para cleanup e TTL (`ShutdownAfter`) para falhas do cliente.

O SDK faz o port-forward nativamente. Este roteiro valida provisionamento, execução, arquivos e separação funcional; isolamento reforçado para código não confiável exige configurar [gVisor/Kata](https://agent-sandbox.sigs.k8s.io/docs/use-cases/gvisor-isolation/). O runtime mínimo não configura persistência nem política de bloqueio de rede.

## Validação da automação

Sintaxe e targets conferidos com `sh -n scripts/poc.sh dev/entrypoint.sh`, `make help` e dry-run dos targets. A checagem de versão foi testada com kind simulado: 0.31.0 é recusado, 0.32.0 e 0.33.0 são aceitos, e versões ilegíveis ou de pré-lançamento são recusadas.

Validação real da IDE em 26/10/2026, no cluster dedicado já provisionado: build Docker com checksums, carga no kind, Sandbox/Pod Ready e PVC Bound; execução com UID 1000; versões de Go, Git, gopls, Delve e extensão Go; `go run` e `gopls check` em código temporário. HTTP sem autenticação redirecionou ao login e o login com a senha do Secret carregou a interface. `dev-down`/`dev-up` preservaram UID do PVC, arquivo sintético do projeto, configurações e senha; o arquivo de teste foi removido. Repetir `dev-up` sem mudanças preservou o UID do Pod. O pool Python permaneceu saudável. A experiência visual do editor e a depuração interativa não foram validadas pelo navegador; arm64 não foi executado.

## CLI Go para ambientes independentes

O CLI de `cmd/sandboxctl` usa os clientes tipados oficiais do Kubernetes e Agent Sandbox v1.0.8 e reaproveita os manifestos da IDE. Requer Go 1.27.3 ou superior. Compile após preparar o cluster e a imagem com `make up` e `make dev-up`:

```sh
make cli
bin/sandboxctl help
bin/sandboxctl create --parallel 2 projeto-a projeto-b
bin/sandboxctl create --ttl 2h temporario
bin/sandboxctl list
bin/sandboxctl password projeto-a
bin/sandboxctl open --port 8081 projeto-a
bin/sandboxctl stop projeto-a
bin/sandboxctl start projeto-a
bin/sandboxctl start --ttl 30m temporario
bin/sandboxctl delete projeto-a
bin/sandboxctl create projeto-a
bin/sandboxctl delete --purge projeto-a projeto-b
```

Flags devem preceder os nomes. Execute na raiz do repositório ou informe `--root /caminho/do/repositorio` após o comando. O CLI usa exclusivamente `.state/kubeconfig`, contexto `kind-agent-sandbox-test`, namespace `default` e confere o UID registrado e Kubernetes 1.36. Não altera o contexto padrão.

`create` exige a imagem já construída e carregada por `make dev-up`, registrada em `.state/dev-image`; não executa Docker, kind ou kubectl. Cada ambiente recebe Sandbox, PVC e Secret próprios, com prefixo `env-` e identificação de propriedade. O Sandbox solicita `spec.service: true`: o controller cria o Service **headless** `env-NOME` e controla sua remoção. O CLI não cria mais um ClusterIP separado. A IDE `go-dev` e o pool Python não são adotados. `list` mostra somente Sandboxes gerenciados pelo CLI, seu estado e expiração absoluta em UTC (`-` indica sem prazo); ambientes removidos com dados preservados não aparecem nessa lista. `Pending`/`Suspending` indicam reconciliação em andamento; condições antigas de outra geração não indicam prontidão.

A senha só é exibida por `password`. `open` acessa diretamente o Pod do Sandbox e mantém o port-forward em primeiro plano, limitado a `127.0.0.1`; Ctrl+C encerra o túnel. `delete` preserva PVC e Secret para reutilização por `create`; `delete --purge` destrói também esses dados somente para os nomes informados. Falhas parciais preservam os recursos para diagnóstico; repetir `create` reaproveita os recursos próprios compatíveis e **preserva o modo e prazo atuais**, mesmo com `--ttl`. Um ambiente suspenso/expirado deve ser retomado por `start`. Alteração de imagem/spec exige `delete` seguido de `create`.

`stop` define `operatingMode: Suspended` e aguarda o Pod desaparecer, preservando Sandbox, PVC e Secret. `start` define `Running` e aguarda Ready; um novo Pod reutiliza os dados. Processos, memória e sessões de depuração são encerrados: reabra-os depois de iniciar.

`create --ttl 2h` define um prazo absoluto a partir da criação do Sandbox. Ao atingir `shutdownTime`, o controller remove Pod e Service e mantém o Sandbox (`shutdownPolicy: Retain`), PVC e Secret. É expiração por tempo decorrido, sem detecção de ociosidade. Sem `--ttl`, novos ambientes não expiram. Durações usam o formato Go (`30m`, `2h`), precisam ser positivas e independem de `--timeout`, que limita a espera do CLI. Se o prazo vencer durante a inicialização, `create` informa expiração e preserva os dados.

`start --ttl 30m` estabelece um novo prazo, inclusive se já estiver ativo. `start` sem `--ttl` limpa o prazo ao retomar um ambiente suspenso/expirado; em um ambiente ativo com prazo vigente, mantém esse prazo.

Ambientes das versões anteriores do CLI são migrados por `create` ou `start` quando correspondem à spec original e têm propriedade válida: habilitam o Service automático e removem o antigo `env-NOME-ide` somente após Ready e validação do novo Service. Dados e credenciais são preservados. Em ambiente suspenso/expirado, `create` mantém o modo e a limpeza do Service legado fica para `start`. Specs modificadas ou recursos de outra origem são recusados para diagnóstico. A IDE `make dev-*` continua com seu Service próprio e não participa dessa migração. Veja a [API oficial v1.0.8](https://github.com/kubernetes-sigs/agent-sandbox/blob/v1.0.8/api/v1beta1/sandbox_types.go) e o [controller v1.0.8](https://github.com/kubernetes-sigs/agent-sandbox/blob/v1.0.8/controllers/sandbox_controller.go).

Os testes automatizados cobrem guardas de cluster/contexto, colisões, propriedade, credenciais e volumes separados, repetição, spec alterada, timeout, preservação/purge e limite de concorrência. Verificações: `go test ./...`, `go vet ./...`, `go test -race ./internal/environments` e `make cli`.

Integração real em 26/10/2026: dois ambientes sintéticos criados em paralelo ficaram Ready; os túneis em portas locais livres exigiram autenticação, aceitaram a senha correta e recusaram a senha do outro ambiente. Repetir create preservou o Pod; delete/recreate preservou UID do PVC, senha e arquivo do projeto; purge removeu somente os recursos selecionados. Os recursos sintéticos foram removidos e os UIDs da IDE/pool, credenciais da IDE e contexto padrão permaneceram iguais. A interface visual e a depuração interativa não foram validadas pelo navegador.

Validação de lifecycle em 27/10/2026: testes unitários de TTL, preservação de modo/prazo, migração legada, ownership e argumentos; `go test ./...`, `go vet ./...` e race nos pacotes CLI/environments. Integração no cluster dedicado com ambiente sintético: Service headless com ownerref, create idempotente, stop repetido e Pod ausente, start Ready com arquivo/PVC/Secret preservados, TTL curto removendo Pod/Service e retendo Sandbox/dados, retomada após expiração e purge seletivo. O ambiente sintético foi purgado; UIDs da IDE/pool, credencial da IDE e contexto padrão permaneceram iguais. A migração de Service legado foi validada com clientes fake, sem execução em ambiente legado real.
