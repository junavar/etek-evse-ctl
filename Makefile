.PHONY: build clean deploy

BINARY_NAME=etek-evse-ctl-arm6
VERSION=$(shell grep 'version =' cmd/etek-evse-ctl/main.go | cut -d '\"' -f 2)

build:
	@echo "Compilando para Raspberry Pi (ARMv6)..."
	GOOS=linux GOARCH=arm GOARM=6 go build -o $(BINARY_NAME) ./cmd/etek-evse-ctl

clean:
	@if [ -f $(BINARY_NAME) ]; then rm $(BINARY_NAME); fi

deploy: build
	@echo "Enviando binario a la Raspberry Pi..."
	# Sustituye 'pi@raspberrypi.local' por tu usuario e IP real
	scp $(BINARY_NAME) pi@192.168.1.153:/home/pi/
	@echo "Hecho. El binario está en /home/pi/$(BINARY_NAME)"

git-push:
	@echo "Subiendo versión $(VERSION) a GitHub..."
	git add .
	git commit -m "Release version $(VERSION)"
	git push origin main

tag:
	@echo "Creando tag v$(VERSION)..."
	git tag -a v$(VERSION) -m "Release version $(VERSION)"
	git push origin v$(VERSION)