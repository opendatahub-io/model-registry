"""Model Registry REST API.

REST API for Model Registry to create and manage ML model metadata

The version of the OpenAPI document: v1alpha3
Generated by OpenAPI Generator (https://openapi-generator.tech)

Do not edit the class manually.
"""  # noqa: E501

from __future__ import annotations

import json
import pprint
import re  # noqa: F401
from typing import Any, ClassVar

from pydantic import BaseModel, ConfigDict, Field, StrictStr
from typing_extensions import Self

from mr_openapi.models.metadata_value import MetadataValue
from mr_openapi.models.registered_model_state import RegisteredModelState


class RegisteredModelCreate(BaseModel):
    """A registered model in model registry. A registered model has ModelVersion children."""  # noqa: E501

    custom_properties: dict[str, MetadataValue] | None = Field(
        default=None,
        description="User provided custom properties which are not defined by its type.",
        alias="customProperties",
    )
    description: StrictStr | None = Field(default=None, description="An optional description about the resource.")
    external_id: StrictStr | None = Field(
        default=None,
        description="The external id that come from the clients’ system. This field is optional. If set, it must be unique among all resources within a database instance.",
        alias="externalId",
    )
    name: StrictStr = Field(
        description="The client provided name of the artifact. This field is optional. If set, it must be unique among all the artifacts of the same artifact type within a database instance and cannot be changed once set."
    )
    owner: StrictStr | None = None
    state: RegisteredModelState | None = None
    __properties: ClassVar[list[str]] = ["customProperties", "description", "externalId", "name", "owner", "state"]

    model_config = ConfigDict(
        populate_by_name=True,
        validate_assignment=True,
        protected_namespaces=(),
    )

    def to_str(self) -> str:
        """Returns the string representation of the model using alias."""
        return pprint.pformat(self.model_dump(by_alias=True))

    def to_json(self) -> str:
        """Returns the JSON representation of the model using alias."""
        # TODO: pydantic v2: use .model_dump_json(by_alias=True, exclude_unset=True) instead
        return json.dumps(self.to_dict())

    @classmethod
    def from_json(cls, json_str: str) -> Self | None:
        """Create an instance of RegisteredModelCreate from a JSON string."""
        return cls.from_dict(json.loads(json_str))

    def to_dict(self) -> dict[str, Any]:
        """Return the dictionary representation of the model using alias.

        This has the following differences from calling pydantic's
        `self.model_dump(by_alias=True)`:

        * `None` is only added to the output dict for nullable fields that
          were set at model initialization. Other fields with value `None`
          are ignored.
        """
        excluded_fields: set[str] = set()

        _dict = self.model_dump(
            by_alias=True,
            exclude=excluded_fields,
            exclude_none=True,
        )
        # override the default output from pydantic by calling `to_dict()` of each value in custom_properties (dict)
        _field_dict = {}
        if self.custom_properties:
            for _key in self.custom_properties:
                if self.custom_properties[_key]:
                    _field_dict[_key] = self.custom_properties[_key].to_dict()
            _dict["customProperties"] = _field_dict
        return _dict

    @classmethod
    def from_dict(cls, obj: dict[str, Any] | None) -> Self | None:
        """Create an instance of RegisteredModelCreate from a dict."""
        if obj is None:
            return None

        if not isinstance(obj, dict):
            return cls.model_validate(obj)

        return cls.model_validate(
            {
                "customProperties": (
                    {_k: MetadataValue.from_dict(_v) for _k, _v in obj["customProperties"].items()}
                    if obj.get("customProperties") is not None
                    else None
                ),
                "description": obj.get("description"),
                "externalId": obj.get("externalId"),
                "name": obj.get("name"),
                "owner": obj.get("owner"),
                "state": obj.get("state"),
            }
        )
