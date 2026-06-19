# Image for the unprivileged web frontend (`quadsync webui`), deployed as the
# `quadstat` tailnet container.
#
# The binary is fully static (CGO_ENABLED=0) and at runtime only listens on a
# TCP port, dials the control socket, and serves an embedded HTML asset — it
# needs no CA certs, /etc/passwd, tzdata, or /tmp. So `scratch` suffices and
# keeps the image at ~the binary size.
#
# scratch defaults to uid 0, which matters: rootless podman then maps the
# container to the cusers-member host user that can open the root:cusers 0660
# control socket. (A distroless base would default to a nonroot uid whose
# mapped subuid is NOT in cusers, and would need an explicit User=0.)
#
# Expects quadsync-linux-arm64 to be built alongside this Dockerfile, e.g.:
#   GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags='-s -w' -o quadsync-linux-arm64 .
FROM scratch
COPY quadsync-linux-arm64 /quadsync
EXPOSE 8765
ENTRYPOINT ["/quadsync", "webui", "--addr", ":8765", "--socket", "/run/quadsync/control.sock"]
