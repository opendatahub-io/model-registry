"""Model Registry REST API.

REST API for Model Registry to create and manage ML model metadata

The version of the OpenAPI document: v1alpha3
Generated by OpenAPI Generator (https://openapi-generator.tech)

Do not edit the class manually.
"""  # noqa: E501

from __future__ import annotations

import json
import pprint
from typing import Any

from pydantic import (
    BaseModel,
    ConfigDict,
    ValidationError,
    field_validator,
)
from typing_extensions import Self

from mr_openapi.models.doc_artifact_create import DocArtifactCreate
from mr_openapi.models.model_artifact_create import ModelArtifactCreate

ARTIFACTCREATE_ONE_OF_SCHEMAS = ["DocArtifactCreate", "ModelArtifactCreate"]


class ArtifactCreate(BaseModel):
    """An Artifact to be created."""

    # data type: ModelArtifactCreate
    oneof_schema_1_validator: ModelArtifactCreate | None = None
    # data type: DocArtifactCreate
    oneof_schema_2_validator: DocArtifactCreate | None = None
    actual_instance: DocArtifactCreate | ModelArtifactCreate | None = None
    one_of_schemas: set[str] = {"DocArtifactCreate", "ModelArtifactCreate"}

    model_config = ConfigDict(
        validate_assignment=True,
        protected_namespaces=(),
    )

    discriminator_value_class_map: dict[str, str] = {}

    def __init__(self, *args, **kwargs) -> None:
        if args:
            if len(args) > 1:
                msg = "If a position argument is used, only 1 is allowed to set `actual_instance`"
                raise ValueError(msg)
            if kwargs:
                msg = "If a position argument is used, keyword arguments cannot be used."
                raise ValueError(msg)
            super().__init__(actual_instance=args[0])
        else:
            super().__init__(**kwargs)

    @field_validator("actual_instance")
    def actual_instance_must_validate_oneof(cls, v):
        ArtifactCreate.model_construct()
        error_messages = []
        match = 0
        # validate data type: ModelArtifactCreate
        if not isinstance(v, ModelArtifactCreate):
            error_messages.append(f"Error! Input type `{type(v)}` is not `ModelArtifactCreate`")
        else:
            match += 1
        # validate data type: DocArtifactCreate
        if not isinstance(v, DocArtifactCreate):
            error_messages.append(f"Error! Input type `{type(v)}` is not `DocArtifactCreate`")
        else:
            match += 1
        if match > 1:
            # more than 1 match
            raise ValueError(
                "Multiple matches found when setting `actual_instance` in ArtifactCreate with oneOf schemas: DocArtifactCreate, ModelArtifactCreate. Details: "
                + ", ".join(error_messages)
            )
        if match == 0:
            # no match
            raise ValueError(
                "No match found when setting `actual_instance` in ArtifactCreate with oneOf schemas: DocArtifactCreate, ModelArtifactCreate. Details: "
                + ", ".join(error_messages)
            )
        return v

    @classmethod
    def from_dict(cls, obj: str | dict[str, Any]) -> Self:
        return cls.from_json(json.dumps(obj))

    @classmethod
    def from_json(cls, json_str: str) -> Self:
        """Returns the object represented by the json string."""
        instance = cls.model_construct()
        error_messages = []
        match = 0

        # use oneOf discriminator to lookup the data type
        _data_type = json.loads(json_str).get("artifactType")
        if not _data_type:
            msg = "Failed to lookup data type from the field `artifactType` in the input."
            raise ValueError(msg)

        # check if data type is `DocArtifactCreate`
        if _data_type == "doc-artifact":
            instance.actual_instance = DocArtifactCreate.from_json(json_str)
            return instance

        # check if data type is `ModelArtifactCreate`
        if _data_type == "model-artifact":
            instance.actual_instance = ModelArtifactCreate.from_json(json_str)
            return instance

        # check if data type is `DocArtifactCreate`
        if _data_type == "DocArtifactCreate":
            instance.actual_instance = DocArtifactCreate.from_json(json_str)
            return instance

        # check if data type is `ModelArtifactCreate`
        if _data_type == "ModelArtifactCreate":
            instance.actual_instance = ModelArtifactCreate.from_json(json_str)
            return instance

        # deserialize data into ModelArtifactCreate
        try:
            instance.actual_instance = ModelArtifactCreate.from_json(json_str)
            match += 1
        except (ValidationError, ValueError) as e:
            error_messages.append(str(e))
        # deserialize data into DocArtifactCreate
        try:
            instance.actual_instance = DocArtifactCreate.from_json(json_str)
            match += 1
        except (ValidationError, ValueError) as e:
            error_messages.append(str(e))

        if match > 1:
            # more than 1 match
            raise ValueError(
                "Multiple matches found when deserializing the JSON string into ArtifactCreate with oneOf schemas: DocArtifactCreate, ModelArtifactCreate. Details: "
                + ", ".join(error_messages)
            )
        if match == 0:
            # no match
            raise ValueError(
                "No match found when deserializing the JSON string into ArtifactCreate with oneOf schemas: DocArtifactCreate, ModelArtifactCreate. Details: "
                + ", ".join(error_messages)
            )
        return instance

    def to_json(self) -> str:
        """Returns the JSON representation of the actual instance."""
        if self.actual_instance is None:
            return "null"

        if hasattr(self.actual_instance, "to_json") and callable(self.actual_instance.to_json):
            return self.actual_instance.to_json()
        return json.dumps(self.actual_instance)

    def to_dict(self) -> dict[str, Any] | DocArtifactCreate | ModelArtifactCreate | None:
        """Returns the dict representation of the actual instance."""
        if self.actual_instance is None:
            return None

        if hasattr(self.actual_instance, "to_dict") and callable(self.actual_instance.to_dict):
            return self.actual_instance.to_dict()
        # primitive type
        return self.actual_instance

    def to_str(self) -> str:
        """Returns the string representation of the actual instance."""
        return pprint.pformat(self.model_dump())
