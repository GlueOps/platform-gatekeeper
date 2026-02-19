NS=glueops-core-gatekeeper
SA=glueops-core-gatekeeper
TOKEN=$(kubectl -n "$NS" create token "$SA" --duration=10m)

curl -sS -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/check?gate=keycloak-prod&ns=nonprod" | jq .
