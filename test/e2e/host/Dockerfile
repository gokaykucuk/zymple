# A throwaway "server": Docker with the containerd image store, plus Caddy.
# The e2e tests run it privileged and install Zymple into it.
FROM docker:29-dind
RUN apk add --no-cache caddy curl
ENV DOCKER_TLS_CERTDIR=""
