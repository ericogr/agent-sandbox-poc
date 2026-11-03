.DEFAULT_GOAL := help

.PHONY: help cli doctor up cluster-up controller router pool smoke status cleanup dev-up dev-open dev-password dev-down

help:
	@printf '%s\n' \
	  'make doctor          Confere ferramentas e versões locais' \
	  'make up              Cria cluster dedicado, controller, router e warm pool' \
	  'make cluster-up      Cria o cluster dedicado (ou valida o gerenciado)' \
	  'make controller      Instala core e extensões v1.0.8' \
	  'make router          Instala router local sem autenticação (ClusterIP)' \
	  'make pool            Aplica template e warm pool Python' \
	  'make smoke           Provisiona, valida e remove claim de teste' \
	  'make status          Mostra componentes e recursos da POC' \
	  'make cli             Compila o CLI Go em bin/sandboxctl' \
	  'make dev-up          Build e provisionamento da IDE Go com volume persistente' \
	  'make dev-open        Abre túnel local da IDE em http://127.0.0.1:8080' \
	  'make dev-password    Mostra a senha da IDE (somente sob demanda)' \
	  'make dev-down        Para a IDE; preserva volume, projeto e senha' \
	  'make cleanup         Remove somente o cluster gerenciado desta POC'

up:
	@$(MAKE) cluster-up
	@$(MAKE) controller
	@$(MAKE) router
	@$(MAKE) pool

doctor cluster-up controller router pool smoke status cleanup dev-up dev-open dev-password dev-down:
	@sh scripts/poc.sh $@

cli:
	@mkdir -p bin
	go build -o bin/sandboxctl ./cmd/sandboxctl
