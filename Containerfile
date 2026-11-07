# Keep in sync with go.mod. CI passes it with --build-arg GO_VERSION=...
ARG GO_VERSION=1.27.3
FROM docker.io/library/golang:${GO_VERSION} AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
# Copy only source code by name, so a stray local file can never end up in the
# image. The dev server and test helpers aren't built.
COPY cmd/photo-vote ./cmd/photo-vote
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /bin/photo-vote ./cmd/photo-vote

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /bin/photo-vote /bin/photo-vote
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/bin/photo-vote"]
