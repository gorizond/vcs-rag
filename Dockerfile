FROM golang:1.27 AS build

WORKDIR /src

# Cache dependencies separately from sources.
COPY go.mod go.su[m] ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o /out/vcs-rag ./cmd/vcs-rag

FROM alpine:3.21

# git is required at runtime: the worker shells out to `git` for mirroring.
RUN apk add --no-cache git ca-certificates

COPY --from=build /out/vcs-rag /vcs-rag

ENTRYPOINT ["/vcs-rag"]
