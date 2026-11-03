TAILWIND ?= tailwindcss
ESBUILD ?= esbuild

css:
	$(TAILWIND) -i web/app.css -o web/static/app.css --minify

js:
	$(ESBUILD) web/ts/app.ts --bundle --minify --target=es2022 --outfile=web/static/app.js
	$(ESBUILD) web/ts/sw.ts --bundle --minify --target=es2022 --outfile=web/static/sw.js

# App icons, rendered from web/icon.svg and web/icon-maskable.svg. The PNGs are
# committed, since the Docker build has no rasteriser; rerun after editing the SVGs.
icons:
	cp web/icon.svg web/static/icons/icon.svg
	rsvg-convert -w 192 -h 192 web/icon.svg -o web/static/icons/icon-192.png
	rsvg-convert -w 512 -h 512 web/icon.svg -o web/static/icons/icon-512.png
	rsvg-convert -w 512 -h 512 web/icon-maskable.svg -o web/static/icons/icon-maskable-512.png
	rsvg-convert -w 180 -h 180 web/icon-maskable.svg -o web/static/icons/apple-touch-icon.png

# Type-check the client code. esbuild strips types without checking them.
check-ts:
	bunx -p typescript@5 tsc -p tsconfig.json
	bunx -p typescript@5 tsc -p tsconfig.sw.json

# Static, like the image, so the same binary also runs inside Playwright's
# container for e2e-webkit.
build: css js
	CGO_ENABLED=0 go build -o signpost .

test:
	go vet ./...
	go test -race ./...

# End-to-end tests in real browsers against a throwaway server (e2e/serve.sh).
# Desktop Chromium runs here; e2e-webkit runs the iPhone/WebKit project in
# Playwright's own image, for hosts without WebKit's system libraries.
e2e: build
	cd e2e && npm ci --no-audit --no-fund && npx playwright test --project=desktop-chromium

e2e-webkit: build
	cd e2e && npm ci --no-audit --no-fund
	docker run --rm --network host --ipc=host -u $$(id -u):$$(id -g) -e HOME=/tmp \
	  -v $(CURDIR):/src -w /src/e2e mcr.microsoft.com/playwright:v1.64.1-noble \
	  npx playwright test --project=iphone-webkit

# Demo data for screenshots and local poking: links and ~90 days of clicks.
demo:
	go run . demo-seed -db dev.db

dev: build
	SIGNPOST_DB=dev.db SIGNPOST_TOKEN=dev SIGNPOST_TRUST_REMOTE_USER=true ./signpost serve

.PHONY: css js icons check-ts build test e2e e2e-webkit demo dev
