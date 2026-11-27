# The page, served by nginx, which also proxies /api and /healthz to the api component.
# nginx 1.31.10-alpine, digest checked on Docker Hub 2026-11-26 (docker buildx imagetools inspect).
FROM nginx:1.31.10-alpine@sha256:7fc0fb4af7bf006aeb31aea88069976060f8f0a0a86b667126742cc17f4e31ae
COPY nginx.conf /etc/nginx/nginx.conf.template
COPY --chmod=0755 docker-entrypoint.sh /web-entrypoint.sh
COPY static/ /usr/share/nginx/html/
USER nginx
EXPOSE 8080
ENTRYPOINT ["/web-entrypoint.sh"]
