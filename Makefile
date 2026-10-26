IMG ?= admission-tap:dev
CAPTURE ?= true
NS  := admission-tap

.PHONY: test build image push certs api-token deploy demo logs api token undeploy

test:
	go vet ./... && go test -race ./...

build:
	CGO_ENABLED=0 go build -o bin/admission-tap .

image:
	docker build -t $(IMG) .

push:
	docker push $(IMG)

certs:
	./hack/gen-certs.sh

# Random bearer token for the query API; generated once, kept in certs/ (gitignored).
api-token: certs/api-token
certs/api-token:
	mkdir -p certs
	umask 077 && openssl rand -hex 32 > $@

# Order matters: the server must be Ready before the webhook configurations exist.
deploy: certs api-token
	kubectl apply -f deploy/00-namespace.yaml
	kubectl -n $(NS) create secret tls admission-tap-tls --cert=certs/tls.crt --key=certs/tls.key \
		--dry-run=client -o yaml | kubectl apply -f -
	kubectl -n $(NS) create secret generic admission-tap-api-token --from-file=token=certs/api-token \
		--dry-run=client -o yaml | kubectl apply -f -
	sed -e 's|__IMAGE__|$(IMG)|' -e 's|__CAPTURE__|$(CAPTURE)|' deploy/10-server.yaml | kubectl apply -f -
	kubectl -n $(NS) rollout status deploy/admission-tap --timeout=120s
	sed "s|__CA_BUNDLE__|$$(base64 < certs/ca.crt | tr -d '\n')|" deploy/20-webhooks.yaml | kubectl apply -f -
ifeq ($(CAPTURE),true)
	sed "s|__CA_BUNDLE__|$$(base64 < certs/ca.crt | tr -d '\n')|" deploy/30-capture.yaml | kubectl apply -f -
else
	kubectl delete validatingwebhookconfiguration admission-tap-capture --ignore-not-found
endif

demo:
	kubectl apply -f examples/namespace.yaml
	kubectl apply -f examples/pod-allowed.yaml
	-kubectl apply -f examples/pod-denied.yaml

logs:
	kubectl -n $(NS) logs deploy/admission-tap -f

# Query API on http://localhost:8080 (try: curl -H "Authorization: Bearer $$(make -s token)" localhost:8080/api/stats).
api:
	kubectl -n $(NS) port-forward svc/admission-tap 8080:80

# Print the in-cluster API token: curl -H "Authorization: Bearer $$(make -s token)" ...
token:
	@kubectl -n $(NS) get secret admission-tap-api-token -o jsonpath='{.data.token}' | base64 -d; echo

undeploy:
	kubectl delete validatingwebhookconfiguration admission-tap-capture --ignore-not-found
	kubectl delete validatingwebhookconfiguration admission-tap --ignore-not-found
	kubectl delete mutatingwebhookconfiguration admission-tap --ignore-not-found
	kubectl delete namespace admission-tap-demo $(NS) --ignore-not-found
