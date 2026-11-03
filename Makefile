TAILWIND ?= tailwindcss

css:
	$(TAILWIND) -i web/app.css -o web/static/app.css --minify

build: css
	go build -o signpost .

dev: build
	SIGNPOST_DB=dev.db SIGNPOST_TOKEN=dev SIGNPOST_TRUST_REMOTE_USER=true ./signpost serve

.PHONY: css build dev
