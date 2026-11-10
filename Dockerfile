# Accquix Register: one static binary on a distroless base (CA certificates included, no shell,
# runs as a non-root user).
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/accquix-register ./cmd/accquix-register \
 && mkdir -p /out/work

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/accquix-register /usr/local/bin/accquix-register
COPY --from=build --chown=65532:65532 /out/work /var/lib/accquix-register
ENV REGISTER_WORK_DIR=/var/lib/accquix-register
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/accquix-register"]
CMD ["serve"]
