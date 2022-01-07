package main

import (
	"encoding/base64"
	"strings"

	"github.com/opensourceways/community-robot-lib/giteeclient"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"
)

const ownerFile = "OWNERS"

func (bot *robot) hasPermission(
	commenter string,
	pr giteeclient.PRInfo,
	log *logrus.Entry,
) (bool, error) {
	commenter = strings.ToLower(commenter)
	p, err := bot.cli.GetUserPermissionsOfRepo(pr.Org, pr.Repo, commenter)
	if err != nil {
		return false, err
	}

	if p.Permission == "admin" || p.Permission == "write" {
		return true, nil
	}

	if bot.isRepoOwners(commenter, pr, log) {
		return true, nil
	}

	return false, nil
}

func (bot *robot) isRepoOwners(
	commenter string,
	pr giteeclient.PRInfo,
	log *logrus.Entry,
) bool {
	v, err := bot.cli.GetPathContent(pr.Org, pr.Repo, ownerFile, pr.BaseRef)
	if err != nil {
		log.Errorf(
			"get file:%s/%s/%s:%s, err:%s",
			pr.Org, pr.Repo, pr.BaseRef, ownerFile, err.Error(),
		)
		return false
	}

	o := decodeOwnerFile(v.Content, log)
	return o.Has(commenter)
}

func decodeOwnerFile(content string, log *logrus.Entry) sets.String {
	owners := sets.NewString()

	c, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		log.WithError(err).Error("decode file")

		return owners
	}

	var m struct {
		Maintainers []string `yaml:"maintainers"`
		Committers  []string `yaml:"committers"`
	}

	if err = yaml.Unmarshal(c, &m); err != nil {
		log.WithError(err).Error("code yaml file")

		return owners
	}

	for _, v := range m.Maintainers {
		owners.Insert(strings.ToLower(v))
	}

	for _, v := range m.Committers {
		owners.Insert(strings.ToLower(v))
	}

	return owners
}
