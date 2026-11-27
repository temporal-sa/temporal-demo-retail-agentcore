#!/bin/sh
# Fills in the API's address and the DNS resolver, then runs nginx in the foreground.
set -eu
: "${API_URL:=http://api:8080}"
RESOLVER="$(awk '/^nameserver/ { print $2; exit }' /etc/resolv.conf)"
export API_URL RESOLVER
envsubst '${API_URL} ${RESOLVER}' < /etc/nginx/nginx.conf.template > /tmp/nginx.conf
exec nginx -c /tmp/nginx.conf -g 'daemon off;'
