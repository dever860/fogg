package config

import (
	"io/ioutil"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	validator "gopkg.in/go-playground/validator.v9"
)

func TestParse(t *testing.T) {
	json := `
	{
		"defaults": {
			"aws_region_backend": "us-west-2",
			"aws_region_provider": "us-west-1",
			"aws_profile_backend": "czi",
			"aws_profile_provider": "czi",
			"infra_s3_bucket": "the-bucket",
			"project": "test-project",
			"shared_infra_base": "../../../../",
			"terraform_version": "0.11.0"
		}
	}`
	r := ioutil.NopCloser(strings.NewReader(json))
	defer r.Close()
	c, e := ReadConfig(r)
	assert.Nil(t, e)
	assert.NotNil(t, c.Defaults)
	assert.Equal(t, "us-west-2", c.Defaults.AWSRegionBackend)
	assert.Equal(t, "us-west-1", c.Defaults.AWSRegionProvider)
	assert.Equal(t, "czi", c.Defaults.AWSProfileBackend)
	assert.Equal(t, "czi", c.Defaults.AWSProfileProvider)
	assert.Equal(t, "the-bucket", c.Defaults.InfraBucket)
	assert.Equal(t, "test-project", c.Defaults.Project)
	assert.Equal(t, "0.11.0", c.Defaults.TerraformVersion)
}

func TestJsonFailure(t *testing.T) {
	json := `foo`
	r := ioutil.NopCloser(strings.NewReader(json))
	defer r.Close()
	c, e := ReadConfig(r)
	assert.Nil(t, c)
	assert.NotNil(t, e)
}

func TestValidation(t *testing.T) {
	json := `{}`
	r := ioutil.NopCloser(strings.NewReader(json))
	defer r.Close()
	c, e := ReadConfig(r)

	assert.NotNil(t, c)
	assert.Nil(t, e)

	e = c.Validate()
	assert.NotNil(t, e)

	_, ok := e.(*validator.InvalidValidationError)
	assert.False(t, ok)

	err, ok := e.(validator.ValidationErrors)
	assert.True(t, ok)
	assert.Len(t, err, 10)
}