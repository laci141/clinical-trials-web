# syntax=docker/dockerfile:1
#
# Stage 1 builds the web server for linux/amd64 from ./main.go. The runtime stage
# copies a PRE-BUILT linux/amd64 CLI binary (bin/clinical-trials-pp-cli-linux),
# produced by vendor-cli.sh — a Windows .exe cannot run in a Linux container.

FROM golang:1.26-alpine AS web-builder
WORKDIR /build
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/server ./main.go

FROM alpine:latest
# ca-certificates: the CLI calls ClinicalTrials.gov / PubMed / OpenAlex / FAERS
# over HTTPS (and LLM providers when a BYOK key is supplied).
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
WORKDIR /app
COPY --from=web-builder /out/server ./server
COPY bin/clinical-trials-pp-cli-linux ./bin/clinical-trials-pp-cli
COPY index.html ./index.html
RUN chmod +x ./bin/clinical-trials-pp-cli
ENV CLI_BIN=/app/bin/clinical-trials-pp-cli
# Server binds 0.0.0.0:$PORT when Render sets $PORT; locally defaults to :8091.
EXPOSE 8091
USER app
CMD ["./server"]
