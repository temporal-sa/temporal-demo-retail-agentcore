#!/bin/sh
# Fills in the API's address and the DNS resolver, then runs nginx in the foreground.
#
# nginx's resolver ignores the search domains in /etc/resolv.conf. Docker Compose answers a
# bare name like "api", but Kubernetes DNS answers only the qualified Service name, so a bare
# host gets the pod's first search domain ("<namespace>.svc.cluster.local") added here.
set -eu
: "${API_URL:=http://api:8080}"
RESOLVER="$(awk '/^nameserver/ { print $2; exit }' /etc/resolv.conf)"
SEARCH="$(awk '/^search/ { print $2; exit }' /etc/resolv.conf)"
host="${API_URL#*://}"
host="${host%%[:/]*}"
case "$host" in
  *.*) ;;
  *) [ -n "$SEARCH" ] && API_URL="$(echo "$API_URL" | sed "s|://$host|://$host.$SEARCH|")" ;;
esac
echo "proxying /api to $API_URL, resolver $RESOLVER"
export API_URL RESOLVER
envsubst '${API_URL} ${RESOLVER}' < /etc/nginx/nginx.conf.template > /tmp/nginx.conf
exec nginx -c /tmp/nginx.conf -g 'daemon off;'
