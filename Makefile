.PHONY: build run test eval demo docker llm-check
build:     ; go build -o alfa-insight .
run:       build ; ./alfa-insight serve
test:      ; go test ./...
eval:      build ; ./alfa-insight eval --companies 6
demo:      build ; ./alfa-insight demo
llm-check: build ; ./alfa-insight llm-check
docker:    ; docker compose up --build
