TAILWIND ?= tailwindcss
ESBUILD ?= esbuild

css:
	$(TAILWIND) -i web/app.css -o web/static/app.css --minify

js:
	$(ESBUILD) web/ts/app.ts --bundle --minify --target=es2022 --outfile=web/static/app.js

# Type-check the client code. esbuild strips types without checking them.
check-ts:
	bunx -p typescript@5 tsc -p tsconfig.json

build: css js
	go build -o signpost .

test:
	go vet ./...
	go test -race ./...

# Demo data for screenshots and local poking: links and ~90 days of clicks.
demo:
	go run . demo-seed -db dev.db

dev: build
	SIGNPOST_DB=dev.db SIGNPOST_TOKEN=dev SIGNPOST_TRUST_REMOTE_USER=true ./signpost serve

.PHONY: css js check-ts build test demo dev
