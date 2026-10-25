# ---- сборка ----
FROM golang:1.24.7-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/alfa-insight .

# ---- запуск ----
FROM alpine:3.20
# poppler-utils — pdftotext для чтения PDF-материалов Альфа-Банка
RUN apk add --no-cache ca-certificates tzdata poppler-utils && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/alfa-insight /app/alfa-insight
COPY configs ./configs
COPY data ./data
RUN mkdir -p output data/state && chown -R app:app /app
USER app
ENV APP_ROOT=/app ADDR=:8080 TZ=Europe/Moscow
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://localhost:8080/api/health || exit 1
ENTRYPOINT ["/app/alfa-insight"]
CMD ["serve"]
