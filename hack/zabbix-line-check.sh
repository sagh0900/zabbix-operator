#!/bin/sh
# Collects what the operator needs to know about a Zabbix version before its release line is
# added to the compatibility ConfigMap: whether every official image is published, the
# database schema version the server ships, and the PostgreSQL range of the official
# requirements. Prints a ConfigMap entry to review; it changes nothing.
#
#   hack/zabbix-line-check.sh 8.2.0 [ubuntu|alpine]
#
# Needs curl and python3; docker (optional) reads the schema version from the server image.
set -eu
VERSION=${1:?usage: $0 <version, e.g. 8.2.0> [flavor]}
FLAVOR=${2:-ubuntu}
LINE=$(echo "$VERSION" | sed -E 's/^([0-9]+\.[0-9]+).*/\1/')
TAG="$FLAVOR-$VERSION"
problems=0

echo "Zabbix $VERSION (line $LINE), images tagged $TAG"
echo
echo "== Official images"
for repo in zabbix-server-pgsql zabbix-web-nginx-pgsql zabbix-web-service zabbix-proxy-sqlite3 zabbix-agent2; do
  code=$(curl -s -o /dev/null -w '%{http_code}' "https://hub.docker.com/v2/namespaces/zabbix/repositories/$repo/tags/$TAG")
  if [ "$code" = 200 ]; then echo "  ok       zabbix/$repo:$TAG"; else echo "  MISSING  zabbix/$repo:$TAG (HTTP $code)"; problems=$((problems + 1)); fi
done

echo
echo "== Database schema (dbversion)"
if command -v docker >/dev/null 2>&1; then
  row=$(docker run --rm --pull=missing --entrypoint sh "zabbix/zabbix-server-pgsql:$TAG" -c \
    'zcat /usr/share/doc/zabbix-server-postgresql/create.sql.gz | grep -i "insert into dbversion"' 2>/dev/null || true)
  mandatory=$(echo "$row" | sed -nE "s/.*'1','([0-9]+)'.*/\1/p")
  if [ -n "$mandatory" ]; then
    level=$((mandatory / 10000))
    echo "  mandatory $mandatory -> schema level $level"
    expect=$(echo "$LINE" | awk -F. '{print $1*100+$2}')
    case "$VERSION" in
      *alpha*|*beta*|*rc*) echo "  pre-release: runs the development schema before the line (the operator expects this)";;
      *) if [ "$level" != "$expect" ]; then echo "  UNEXPECTED: line $LINE should report level $expect"; problems=$((problems + 1)); fi;;
    esac
  else
    echo "  could not read dbversion from zabbix/zabbix-server-pgsql:$TAG"; problems=$((problems + 1))
  fi
else
  echo "  skipped (docker not available)"
fi

echo
echo "== PostgreSQL requirements (official documentation)"
URL="https://www.zabbix.com/documentation/$LINE/en/manual/installation/requirements"
range=$(curl -sL "$URL" | python3 -c '
import html, re, sys
t = re.sub(r"\s+", " ", html.unescape(re.sub(r"<[^>]+>", " ", sys.stdin.read())))
m = re.search(r"PostgreSQL (\d+)\.\d+-(\d+)\.X Required", t)
print(f"{m.group(1)} {m.group(2)}" if m else "")')
if [ -n "$range" ]; then
  MIN=${range% *}; MAX=${range#* }
  echo "  PostgreSQL $MIN to $MAX   ($URL)"
else
  MIN="<min>"; MAX="<max>"
  echo "  not found on $URL; read the requirements page and fill in the range"; problems=$((problems + 1))
fi

echo
echo "== Proposed entry for the zabbix-operator-compatibility ConfigMap (key compatibility.yaml)"
cat <<YAML
lines:
  - line: "$LINE"
    minPostgres: $MIN
    maxPostgres: $MAX
YAML
echo
if [ "$problems" -gt 0 ]; then
  echo "$problems problem(s) above: resolve them before adding the line."
  exit 1
fi
echo "Before upgrading a system to this line: read the line's upgrade notes, take a full CNPG"
echo "backup, and rehearse the upgrade on a restored copy of the database."
