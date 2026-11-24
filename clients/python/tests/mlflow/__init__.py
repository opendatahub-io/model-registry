"""Tests for the MLflow-compatible backend.

These tests run only with --mlflow-backend: the managed fixtures own
the adapter subprocess, its isolated port, and the disposable
compatibility database, and never touch the externally managed native
MLflow server beyond models the tests create.
"""
