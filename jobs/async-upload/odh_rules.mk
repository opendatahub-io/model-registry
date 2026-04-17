.PHONY: deploy-mr-odh
deploy-mr-odh:
	cd ../../ && ./scripts/deploy_on_odh.sh

.PHONY: undeploy-mr-odh
undeploy-mr-odh:
	cd ../../ && ./scripts/undeploy_on_odh.sh

.PHONY: test-e2e-odh-async-jobs
test-e2e-odh-async-jobs: deploy-mr-odh deploy-local-registry deploy-test-minio
	@echo "Running Async Jobs e2e tests..."
	@../../scripts/odh_env.sh

.PHONY: test-e2e-odh-async-cleanup
test-e2e-odh-async-jobs-cleanup: undeploy-mr-odh undeploy-minio undeploy-local-kind-registry
	@echo "Cleaning up port-forward processes..."
	@if [ -f .port-forwards.pid ]; then \
		kill $$(cat .port-forwards.pid) || true; \
		rm -f .port-forwards.pid; \
	fi
