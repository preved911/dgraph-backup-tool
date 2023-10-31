package export

import (
	"context"
	"fmt"
	"net/url"

	"github.com/hasura/go-graphql-client"
)

func NewClient(endpoint, dest string, opts ...Option) (*Client, error) {
	_, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}

	c := &Client{
		cli: graphql.NewClient(endpoint, nil),
		in: ExportInput{
			Format:      "rdf",
			Destination: graphql.String(dest),
		},
	}

	for _, opt := range opts {
		opt(c)
	}

	return c, nil
}

type Client struct {
	cli *graphql.Client
	in  ExportInput
}

// https://github.com/dgraph-io/dgraph/blob/v23.1.0/protos/pb/pb.pb.go#L4946
type ExportInput struct {
	Format       graphql.String  `json:"format"`
	Destination  graphql.String  `json:"destination"`
	AccessKey    graphql.String  `json:"accessKey"`
	SecretKey    graphql.String  `json:"secretKey"`
	SessionToken graphql.String  `json:"sessionToken"`
	Anonymous    graphql.Boolean `json:"anonymous"`
	Namespace    graphql.Int     `json:"namespace"`
}

type Option func(*Client)

func WithAccessKey(value string) Option {
	return func(c *Client) {
		c.in.AccessKey = graphql.String(value)
	}
}

func WithSecretKey(value string) Option {
	return func(c *Client) {
		c.in.SecretKey = graphql.String(value)
	}
}

func WithAnonymous(value bool) Option {
	return func(c *Client) {
		c.in.Anonymous = graphql.Boolean(value)
	}
}

func (c *Client) Export(ctx context.Context) (*ExportOutput, error) {
	vars := map[string]interface{}{
		"input": c.in,
	}

	var mutation struct {
		ExportOutput `graphql:"export(input: $input)"`
	}

	if err := c.cli.Mutate(context.Background(), &mutation, vars); err != nil {
		return nil, err
	}

	if resp := mutation.ExportOutput.Response; resp.Code != "Success" {
		return nil, fmt.Errorf(
			`export finished with unseccessfull code "%s": %s`, resp.Code, resp.Message)
	}

	return &mutation.ExportOutput, nil
}

// https://github.com/dgraph-io/dgraph/blob/v23.1.0/protos/pb/pb.pb.go#L5063
type ExportOutput struct {
	Response struct {
		Message graphql.String
		Code    graphql.String
	}

	ExportedFiles []graphql.String
}

func (resp *ExportOutput) GetFiles() []string {
	files := make([]string, 0)
	for _, file := range resp.ExportedFiles {
		files = append(files, string(file))
	}

	return files
}
