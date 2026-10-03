FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
ARG VERSION=0.0.0-dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags="-s -w \
        -X github.com/sagh0900/zabbix-operator/internal/version.Version=${VERSION} \
        -X github.com/sagh0900/zabbix-operator/internal/version.GitCommit=${GIT_COMMIT} \
        -X github.com/sagh0900/zabbix-operator/internal/version.BuildDate=${BUILD_DATE}" \
      -o /manager ./cmd/manager

FROM gcr.io/distroless/static:nonroot
COPY --from=build /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
