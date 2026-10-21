# The binary is built on the host (make build: static, CGO off) and copied in, so the image build needs no Go
# toolchain. kubectl is included for the kubectl fallback tool.
FROM alpine:3
ARG KUBECTL_VERSION=v1.36.4
RUN apk add --no-cache ca-certificates \
 && wget -qO /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
 && chmod 0755 /usr/local/bin/kubectl
COPY bin/kharness /usr/local/bin/kharness
USER 65532:65532
ENTRYPOINT ["kharness"]
CMD ["serve"]
