.PHONY: deploy-mr-odh
deploy-mr-odh:
	cd ../../ && ./scripts/deploy_on_odh.sh

.PHONY: undeploy-mr-odh
undeploy-mr-odh:
	cd ../../ && ./scripts/undeploy_on_odh.sh

.PHONY: test-e2e-odh
test-e2e-odh:
	@echo "Running tests..."
	@set -a; . ../../scripts/manifests/seaweedfs/.env; set +a; \
	mkdir -p ../../results; \
	export AUTH_TOKEN=$$(kubectl config view --raw -o jsonpath="{.users[?(@.name==\"$$(kubectl config view -o jsonpath="{.contexts[?(@.name==\"$$(kubectl config current-context)\")].context.user}")\")].user.token}") && \
	export VERIFY_SSL=False && \
	export MR_NAMESPACE=$$(kubectl get datasciencecluster default-dsc -o jsonpath='{.spec.components.modelregistry.registriesNamespace}') && \
	MR_HOST=$$(kubectl get route -n "$$MR_NAMESPACE" model-registry-https -o jsonpath='{.spec.host}') && \
	export MR_URL="https://$${MR_HOST}:443" && \
	poetry install --all-extras && poetry run pytest --e2e -svvv -rA --html=../../results/report.html --junit-xml=../../results/xunit_report.xml --self-contained-html -o junit_suite_name=odh-model-registry && \
	rm -f ../../scripts/manifests/seaweedfs/.env

.PHONY: test-e2e-port-cleanup
test-e2e-port-cleanup:
	@echo "Cleaning up port-forward processes..."
	@if [ -f .port-forwards.pid ]; then \
		kill $$(cat .port-forwards.pid) || true; \
		rm -f .port-forwards.pid; \
	fi

.PHONY: test-fuzz-odh
test-fuzz-odh:
	@echo "Starting test-fuzz"
	poetry install --all-extras
	@set -a; . ../../scripts/manifests/seaweedfs/.env; set +a; \
	export VERIFY_SSL=False && \
	export AUTH_TOKEN=$$(kubectl config view --raw -o jsonpath="{.users[?(@.name==\"$$(kubectl config view -o jsonpath="{.contexts[?(@.name==\"$$(kubectl config current-context)\")].context.user}")\")].user.token}") && \
	export MR_NAMESPACE=$$(kubectl get datasciencecluster default-dsc -o jsonpath='{.spec.components.modelregistry.registriesNamespace}') && \
	MR_HOST=$$(kubectl get route -n "$$MR_NAMESPACE" model-registry-https -o jsonpath='{.spec.host}') && \
	export MR_URL="https://$${MR_HOST}:443" && \
	poetry run pytest --fuzz -svvv --hypothesis-show-statistics tests/fuzz_api -rA --html=../../results/report.html --junit-xml=../../results/xunit_report.xml --self-contained-html -o junit_suite_name=odh-model-registry
	@exit $$STATUS
