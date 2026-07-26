VERSION ?= dev
OUT := dist/cpa-apply-patch-v$(VERSION).so

.PHONY: build clean

build:
	mkdir -p dist
	CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -o $(OUT) .
	rm -f $(basename $(OUT)).h
	@echo built: $(OUT)

clean:
	rm -rf dist
