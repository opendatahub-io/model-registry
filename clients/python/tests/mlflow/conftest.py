import uuid

import pytest

from .adapter import TEST_MODEL_PREFIX, MLflowStack


@pytest.fixture(scope="session")
def mlflow_stack(managed_adapter: MLflowStack) -> MLflowStack:
    """The session-owned MLflow backend stack ( see managed_adapter )."""
    return managed_adapter


@pytest.fixture
def mlflow_model_name(mlflow_stack: MLflowStack) -> str:
    """A unique, legal native model name owned by one test."""
    name = f"{TEST_MODEL_PREFIX}{uuid.uuid4().hex[:12]}"
    mlflow_stack.record_owned_model(name)
    return name


@pytest.fixture
def native_verifier(mlflow_stack: MLflowStack):
    """Direct native REST access for independent verification.

    Every list fetches all pages; nothing assumes a single response
    contains every version.
    """
    return mlflow_stack
