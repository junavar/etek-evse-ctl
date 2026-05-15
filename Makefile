# Variables de despliegue
RPI_USER = pi
RPI_IP   = 192.168.1.153
RPI_DEST = /home/pi/

# Variables de GitHub
GITHUB_REPO = https://github.com/junavar/etek-evse-ctl.git
GITHUB_BRANCH = main

BINARY_NAME = etek-evse-ctl-arm6
GO_VARS = GOOS=linux GOARCH=arm GOARM=6 GO111MODULE=on

# Extraer la versión desde la constante en main.go (nótese 'version' en minúscula)
VERSION = $(shell grep 'version =' main.go | head -n 1 | cut -d '"' -f 2)

.PHONY: all build deploy clean push_github version init_repo

all: build

build:
	@echo "Compilando $(BINARY_NAME) v$(VERSION) para ARMv6..."
	$(GO_VARS) go build -o $(BINARY_NAME) .

init_repo:
	git init
	git remote add origin $(GITHUB_REPO)
	git branch -M $(GITHUB_BRANCH)
	@echo "Repositorio inicializado y vinculado a $(GITHUB_REPO)"

push_github:
	@read -p "Introduce el mensaje de commit: " commit_message; \
	git add .; \
	git commit -m "$$commit_message"; \
	git tag -a v$(VERSION) -m "Release v$(VERSION)"; \
	git push origin $(GITHUB_BRANCH); \
	git push origin v$(VERSION)

deploy: build
	@echo "Copiando binario a la Raspberry Pi..."
	scp $(BINARY_NAME) $(RPI_USER)@$(RPI_IP):$(RPI_DEST)
	@echo "Despliegue de etek-evse-ctl completado."

clean:
	rm -f $(BINARY_NAME)
	@echo "Limpieza completada."

version:
	@echo "Versión actual: $(VERSION)"