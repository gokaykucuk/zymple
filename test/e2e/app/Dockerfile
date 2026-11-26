# The app the e2e tests deploy: a static HTTP server that reports its version.
# HEALTHY=0 builds an image whose health check never passes.
FROM busybox:1.37
ARG VERSION=dev
ARG HEALTHY=1
RUN mkdir -p /www && echo "$VERSION" > /www/version \
 && if [ "$HEALTHY" = 1 ]; then echo ok > /www/healthz; fi \
 && adduser -D -u 10001 app
USER app
EXPOSE 3000
CMD ["httpd", "-f", "-p", "3000", "-h", "/www"]
