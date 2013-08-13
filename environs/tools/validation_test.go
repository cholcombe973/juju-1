// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package tools

import (
	"net/http"

	gc "launchpad.net/gocheck"

	"launchpad.net/juju-core/environs/config"
	"launchpad.net/juju-core/environs/simplestreams"
	coretesting "launchpad.net/juju-core/testing"
)

type ValidateSuite struct {
	home      *coretesting.FakeHome
	oldClient *http.Client
}

var _ = gc.Suite(&ValidateSuite{})

func (s *ValidateSuite) makeLocalMetadata(c *gc.C, version, region, series, endpoint string) error {
	tm := ToolsMetadata{
		Version:  version,
		Release:  series,
		Arch:     "amd64",
		Path:     "/tools/tools.tar.gz",
		Size:     1234,
		FileType: "tar.gz",
		Hash:     "f65a92b3b41311bdf398663ee1c5cd0c",
	}
	cloudSpec := simplestreams.CloudSpec{
		Region:   region,
		Endpoint: endpoint,
	}
	_, err := MakeBoilerplate("", series, &tm, &cloudSpec, false)
	if err != nil {
		return err
	}

	t := &http.Transport{}
	t.RegisterProtocol("file", http.NewFileTransport(http.Dir("/")))
	s.oldClient = simplestreams.SetHttpClient(&http.Client{Transport: t})
	return nil
}

func (s *ValidateSuite) SetUpTest(c *gc.C) {
	s.home = coretesting.MakeEmptyFakeHome(c)
}

func (s *ValidateSuite) TearDownTest(c *gc.C) {
	s.home.Restore()
	if s.oldClient != nil {
		simplestreams.SetHttpClient(s.oldClient)
	}
}

func (s *ValidateSuite) TestMatch(c *gc.C) {
	s.makeLocalMetadata(c, "1.11.2", "region-2", "raring", "some-auth-url")
	metadataDir := config.JujuHomePath("")
	params := &MetadataLookupParams{
		Version:       "1.11.2",
		Region:        "region-2",
		Series:        "raring",
		Architectures: []string{"amd64"},
		Endpoint:      "some-auth-url",
		BaseURLs:      []string{"file://" + metadataDir},
	}
	versions, err := ValidateToolsMetadata(params)
	c.Assert(err, gc.IsNil)
	c.Assert(versions, gc.DeepEquals, []string{"1.11.2-raring-amd64"})
}

func (s *ValidateSuite) TestNoMatch(c *gc.C) {
	s.makeLocalMetadata(c, "1.11.2", "region-2", "raring", "some-auth-url")
	metadataDir := config.JujuHomePath("")
	params := &MetadataLookupParams{
		Version:       "1.11.2",
		Region:        "region-2",
		Series:        "precise",
		Architectures: []string{"amd64"},
		Endpoint:      "some-auth-url",
		BaseURLs:      []string{"file://" + metadataDir},
	}
	_, err := ValidateToolsMetadata(params)
	c.Assert(err, gc.Not(gc.IsNil))
}
