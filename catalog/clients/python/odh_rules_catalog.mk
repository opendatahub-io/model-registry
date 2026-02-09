.PHONY: test-e2e-catalog
test-e2e-catalog:
	@echo "Running catalog tests..."
	export MC_NAMESPACE=$$(kubectl get datasciencecluster default-dsc -o jsonpath='{.spec.components.modelregistry.registriesNamespace}') && \
	export CATALOG_URL="https://$$(kubectl get route -n "$$MC_NAMESPACE" model-catalog-https -o 'jsonpath={.status.ingress[0].host}')/" && \
	export AUTH_TOKEN=$$(kubectl config view --raw -o jsonpath="{.users[?(@.name==\"$$(kubectl config view -o jsonpath="{.contexts[?(@.name==\"$$(kubectl config current-context)\")].context.user}")\")].user.token}") && \
	export VERIFY_SSL=False && \
	export KIND_CLUSTER=False && \
	poetry install --all-extras && poetry run pytest tests --e2e -svv -rA