// Code generated by protoc-gen-goext. DO NOT EDIT.

package iam

import (
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
)

func (m *ApiKey) SetId(v string) {
	m.Id = v
}

func (m *ApiKey) SetServiceAccountId(v string) {
	m.ServiceAccountId = v
}

func (m *ApiKey) SetCreatedAt(v *timestamppb.Timestamp) {
	m.CreatedAt = v
}

func (m *ApiKey) SetDescription(v string) {
	m.Description = v
}
