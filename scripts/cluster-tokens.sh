#!/usr/bin/env bash
# Fetches the cluster's own credentials from the platform. enrol spends the
# one-time enrolment token for the report token and the cluster repository's
# token; renew trades the report token for fresh tokens for the cluster
# repository and each app slice's repository. Each token goes into the Secret
# that uses it.
# Usage: PLATFORM_URL=<https origin> SUBSCRIPTION_ID=<id> REPO_URL=<https url> cluster-tokens.sh enrol|renew
#   TOKEN_FILE names the bearer token's file, /var/run/mitstack/token by
#   default, and CALL_ATTEMPTS how often a 429, a 503 or no answer is retried, 20.
# It exists because nothing may dial into the cluster, so the cluster asks for
# what it needs.
set -euo pipefail

readonly token_file=${TOKEN_FILE:-/var/run/mitstack/token}
readonly attempts=${CALL_ATTEMPTS:-20}
readonly node_namespace=mitstack-node report_secret=mitstack-report-token
readonly argocd_namespace=argocd repository_secret=cluster-repository
readonly kube_api=${KUBE_API:-https://kubernetes.default.svc}
readonly kube_account=${KUBE_ACCOUNT_DIR:-/var/run/secrets/kubernetes.io/serviceaccount}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() {
  echo "cluster-tokens: $*" >&2
  exit 1
}

# post <path> <body file> prints the platform's status and writes its answer to $work/answer, retrying a passing refusal.
post() {
  local attempt status
  [ -s "$token_file" ] || fail "no bearer token at $token_file"
  printf 'Authorization: Bearer %s\n' "$(cat "$token_file")" > "$work/headers"
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    status=$(curl -sS --proto '=https' --tlsv1.2 --connect-timeout 5 --max-time 30 -X POST \
      -H @"$work/headers" -H 'Content-Type: application/json' --data-binary @"$2" \
      -o "$work/answer" -w '%{http_code}' "$PLATFORM_URL$1" || echo 000)
    case $status in
      429 | 503 | 000) sleep 15 ;;
      *) echo "$status"; return 0 ;;
    esac
  done
  echo "$status"
}

write_report_token() {
  jq -j .reportToken "$work/answer" > "$work/report-token"
  kubectl create secret generic "$report_secret" -n "$node_namespace" --from-file=token="$work/report-token" \
    --dry-run=client -o yaml | kubectl apply -f - > /dev/null
}

# write_repository_token gives Argo CD the cluster repository with the token as its password.
write_repository_token() {
  jq -j .repositoryToken "$work/answer" > "$work/repository-token"
  kubectl create secret generic "$repository_secret" -n "$argocd_namespace" \
    --from-literal=type=git --from-literal=url="$REPO_URL" --from-literal=username=x-access-token \
    --from-file=password="$work/repository-token" --dry-run=client -o yaml |
    kubectl label --local -f - argocd.argoproj.io/secret-type=repository -o yaml | kubectl apply -f - > /dev/null
}

enrol() {
  local status
  jq -n --arg id "$SUBSCRIPTION_ID" '{subscriptionId: $id}' > "$work/body"
  status=$(post /v1/cluster-enrolments "$work/body")
  case $status in
    200)
      write_report_token
      write_repository_token
      echo "cluster-tokens: enrolled subscription $SUBSCRIPTION_ID"
      ;;
    409) echo "cluster-tokens: subscription $SUBSCRIPTION_ID enrolled already" ;;
    *) fail "the platform answered $status to the enrolment: $(jq -r '.error // empty' "$work/answer" 2> /dev/null)" ;;
  esac
}

# patch_app_repository sends the password alone as a merge patch, since a slice's Role grants patch and no read.
patch_app_repository() {
  local name=$1 token=$2 status
  jq -n --arg password "$token" '{stringData: {password: $password}}' > "$work/patch"
  printf 'Authorization: Bearer %s\n' "$(cat "$kube_account/token")" > "$work/kube-headers"
  status=$(curl -sS --proto '=https' --cacert "$kube_account/ca.crt" --max-time 10 -X PATCH \
    -H @"$work/kube-headers" -H 'Content-Type: application/merge-patch+json' --data-binary @"$work/patch" \
    -o /dev/null -w '%{http_code}' "$kube_api/api/v1/namespaces/$argocd_namespace/secrets/$name" || echo 000)
  if [ "$status" != 200 ]; then
    echo "cluster-tokens: could not patch $name in $argocd_namespace: HTTP $status; the next run tries again" >&2
  fi
}

# patch_app_repositories names each slice's Secret app-repository-<app> after its project app-<app>, as the slices commit it.
patch_app_repositories() {
  local project token
  while IFS=$'\t' read -r project token; do
    [[ $project =~ ^app-([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)$ ]] || fail "the renewal names the unusable project $project"
    patch_app_repository "app-repository-${BASH_REMATCH[1]}" "$token"
  done < <(jq -r '.appRepositories // [] | .[] | [.project, .repositoryToken] | @tsv' "$work/answer")
}

renew() {
  local status
  echo '{}' > "$work/body"
  status=$(post /v1/cluster-repository-tokens "$work/body")
  [ "$status" = 200 ] || fail "the platform answered $status to the repository token renewal"
  write_repository_token
  patch_app_repositories
  echo "cluster-tokens: renewed the repository tokens"
}

main() {
  : "${PLATFORM_URL:?}" "${SUBSCRIPTION_ID:?}" "${REPO_URL:?}"
  case ${1:-} in
    enrol) enrol ;;
    renew) renew ;;
    *) fail "usage: cluster-tokens.sh enrol|renew" ;;
  esac
}

main "$@"
