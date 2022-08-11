package main

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"

	sdk "github.com/opensourceways/go-gitee/gitee"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"
)

const (
	msgPRConflicts        = "PR conflicts to the target branch."
	msgMissingLabels      = "PR does not have these lables: %s"
	msgInvalidLabels      = "PR should remove these labels: %s"
	msgNotEnoughLGTMLabel = "PR needs %d lgtm labels and now gets %d"
	msgFrozenWithOwner    = "The target branch of PR has been frozen and it can be merge only by branch owners: %s"
	legalLabelsAddedBy    = "openeuler-ci-bot"
)

var regCheckPr = regexp.MustCompile(`(?mi)^/check-pr\s*$`)

func (bot *robot) handleCheckPR(e *sdk.NoteEvent, cfg *botConfig, log *logrus.Entry) error {
	if !e.IsPullRequest() ||
		!e.IsPROpen() ||
		!e.IsCreatingCommentEvent() ||
		!regCheckPr.MatchString(e.GetComment().GetBody()) {
		return nil
	}

	return bot.tryMerge(e, cfg, true, log)
}

func (bot *robot) tryMerge(e *sdk.NoteEvent, cfg *botConfig, addComment bool, log *logrus.Entry) error {
	org, repo := e.GetOrgRepo()

	h := mergeHelper{
		cfg:     cfg,
		org:     org,
		repo:    repo,
		cli:     bot.cli,
		pr:      e.GetPullRequest(),
		trigger: e.GetCommenter(),
	}

	if r, ok := h.canMerge(log); !ok {
		if len(r) > 0 && addComment {
			return bot.cli.CreatePRComment(
				org, repo, e.GetPRNumber(),
				fmt.Sprintf(
					"@%s , this pr is not mergeable and the reasons are below:\n%s",
					e.GetCommenter(), strings.Join(r, "\n"),
				),
			)
		}

		return nil
	}

	return h.merge()
}

func (bot *robot) handleLabelUpdate(e *sdk.PullRequestEvent, cfg *botConfig, log *logrus.Entry) error {
	if sdk.GetPullRequestAction(e) != sdk.PRActionUpdatedLabel {
		return nil
	}

	org, repo := e.GetOrgRepo()
	h := mergeHelper{
		cfg:  cfg,
		org:  org,
		repo: repo,
		cli:  bot.cli,
		pr:   e.GetPullRequest(),
	}

	if _, ok := h.canMerge(log); ok {
		return h.merge()
	}

	return nil
}

type mergeHelper struct {
	pr  *sdk.PullRequestHook
	cfg *botConfig

	org     string
	repo    string
	trigger string

	cli iClient
}

func (m *mergeHelper) merge() error {
	number := m.pr.Number

	if m.pr.NeedReview || m.pr.NeedTest {
		v := int32(0)
		p := sdk.PullRequestUpdateParam{
			AssigneesNumber: &v,
			TestersNumber:   &v,
		}

		if _, err := m.cli.UpdatePullRequest(m.org, m.repo, number, p); err != nil {
			return err
		}
	}

	desc := m.genMergeDesc()

	if m.org == "openeuler" && m.repo == "kernel" {
		return m.cli.MergePR(
			m.org, m.repo, number,
			sdk.PullRequestMergePutParam{
				MergeMethod: string(m.cfg.MergeMethod),
				Description: fmt.Sprintf("\n%s \n \n%s \n \n%s %s",
					fmt.Sprintf("Merge Pull Request from: @%s", m.pr.User.GetLogin()),
					m.pr.Body, fmt.Sprintf("Link:%s", m.pr.GetHtmlURL()), desc),
			},
		)
	}

	return m.cli.MergePR(
		m.org, m.repo, number,
		sdk.PullRequestMergePutParam{
			MergeMethod: string(m.cfg.MergeMethod),
			Description: fmt.Sprintf("\n%s", desc),
		},
	)
}

func (m *mergeHelper) canMerge(log *logrus.Entry) ([]string, bool) {
	if !m.pr.GetMergeable() {
		return []string{msgPRConflicts}, false
	}

	org := m.org
	repo := m.repo
	number := m.pr.GetNumber()

	ops, err := m.cli.ListPROperationLogs(org, repo, number)
	if err != nil {
		return []string{}, false
	}

	for label := range m.getPRLabels() {
		for _, l := range m.cfg.LabelsNotAllowMerge {
			if l == label {
				return []string{}, false
			}
		}
	}

	if r := isLabelMatched(m.getPRLabels(), m.cfg, ops, log); len(r) > 0 {
		return r, false
	}

	freeze, err := m.getFreezeInfo(log)
	if err != nil {
		return nil, false
	}

	if freeze == nil || !freeze.isFrozen() {
		return nil, true
	}

	if m.trigger == "" {
		return nil, false
	}

	if freeze.isOwner(m.trigger) {
		return nil, true
	}

	return []string{
		fmt.Sprintf(msgFrozenWithOwner, strings.Join(freeze.Owner, ", ")),
	}, false
}

func (m *mergeHelper) getFreezeInfo(log *logrus.Entry) (*freezeItem, error) {
	branch := m.pr.GetBase().GetRef()
	for _, v := range m.cfg.FreezeFile {
		fc, err := m.getFreezeContent(v)
		if err != nil {
			log.Errorf("get freeze file:%s, err:%s", v.toString(), err.Error())
			return nil, err
		}

		if v := fc.getFreezeItem(m.org, branch); v != nil {
			return v, nil
		}
	}

	return nil, nil
}

func (m *mergeHelper) getFreezeContent(f freezeFile) (freezeContent, error) {
	var fc freezeContent

	c, err := m.cli.GetPathContent(f.Owner, f.Repo, f.Path, f.Branch)
	if err != nil {
		return fc, err
	}

	b, err := base64.StdEncoding.DecodeString(c.Content)
	if err != nil {
		return fc, err
	}

	err = yaml.Unmarshal(b, &fc)

	return fc, err
}

func (m *mergeHelper) getPRLabels() sets.String {
	if m.trigger == "" {
		return m.pr.LabelsToSet()
	}

	prLabels, err := m.cli.GetPRLabels(m.org, m.repo, m.pr.GetNumber())
	if err != nil {
		return m.pr.LabelsToSet()
	}

	labels := sets.NewString()
	for _, v := range prLabels {
		labels.Insert(v.Name)
	}

	return labels
}

func (m *mergeHelper) genMergeDesc() string {
	comments, err := m.cli.ListPRComments(m.org, m.repo, m.pr.Number)
	if err != nil || len(comments) == 0 {
		return ""
	}

	f := func(comment sdk.PullRequestComments, reg *regexp.Regexp) bool {
		return reg.MatchString(comment.Body) &&
			comment.UpdatedAt == comment.CreatedAt &&
			comment.User.Login != m.pr.User.Login
	}

	f2 := func(comment sdk.PullRequestComments, reg *regexp.Regexp) bool {
		return reg.MatchString(comment.Body) &&
			comment.User.Login != m.pr.User.Login
	}

	reviewers := sets.NewString()
	signers := sets.NewString()

	org, repo := m.org, m.repo
	for _, c := range comments {
		if org == "openeuler" && repo == "kernel" {
			if f2(c, regAddLgtm) {
				reviewers.Insert(c.User.Login)
			}

			if f2(c, regAddApprove) {
				signers.Insert(c.User.Login)
			}
		}
		if f(c, regAddLgtm) {
			reviewers.Insert(c.User.Login)
		}

		if f(c, regAddApprove) {
			signers.Insert(c.User.Login)
		}
	}

	if len(signers) == 0 && len(reviewers) == 0 {
		return ""
	}

	// kernel return the name and email address
	if org == "openeuler" && repo == "kernel" {
		content, err := m.cli.GetPathContent("openeuler", "community", "sig/Kernel/sig-info.yaml", "master")
		if err != nil {
			return ""
		}

		c, err := base64.StdEncoding.DecodeString(content.Content)
		if err != nil {
			return ""
		}

		var s SigInfos

		if err = yaml.Unmarshal(c, &s); err != nil {
			return ""
		}

		nameEmail := make(map[string]string, len(s.Maintainers))
		for _, ms := range s.Maintainers {
			nameEmail[ms.GiteeID] = fmt.Sprintf("%s <%s>", ms.Name, ms.Email)
		}

		for _, i := range s.Repositories {
			for _, j := range i.Committers {
				nameEmail[j.GiteeID] = fmt.Sprintf("%s <%s>", j.Name, j.Email)
			}
		}

		reviewersInfo := sets.NewString()
		for r, _ := range reviewers {
			if v, ok := nameEmail[r]; ok {
				reviewersInfo.Insert(v)
			}
		}

		signersInfo := sets.NewString()
		for s, _ := range signers {
			if v, ok := nameEmail[s]; ok {
				signersInfo.Insert(v)
			}
		}

		reviewedUserInfo := make([]string, 0)
		for _, item := range reviewersInfo.UnsortedList() {
			reviewedUserInfo = append(reviewedUserInfo, fmt.Sprintf("Reviewed-by: %s \n", item))
		}

		signedOffUserInfo := make([]string, 0)
		for _, item := range signersInfo.UnsortedList() {
			signedOffUserInfo = append(signedOffUserInfo, fmt.Sprintf("Signed-off-by: %s \n", item))
		}

		return fmt.Sprintf(
			"\n%s%s",
			strings.Join(reviewedUserInfo, ""),
			strings.Join(signedOffUserInfo, ""),
		)
	}

	return fmt.Sprintf(
		"From: @%s \nReviewed-by: @%s \nSigned-off-by: @%s \n",
		m.pr.User.Login,
		strings.Join(reviewers.UnsortedList(), ", @"),
		strings.Join(signers.UnsortedList(), ", @"),
	)
}

func isLabelMatched(labels sets.String, cfg *botConfig, ops []sdk.OperateLog, log *logrus.Entry) []string {
	var reasons []string

	needs := sets.NewString(approvedLabel)
	needs.Insert(cfg.LabelsForMerge...)

	if ln := cfg.LgtmCountsRequired; ln == 1 {
		needs.Insert(lgtmLabel)
	} else {
		v := getLGTMLabelsOnPR(labels)
		if n := uint(len(v)); n < ln {
			reasons = append(reasons, fmt.Sprintf(msgNotEnoughLGTMLabel, ln, n))
		}
	}

	s := checkLabelsLegal(labels, needs, ops, log)
	if s != "" {
		reasons = append(reasons, s)
	}

	if v := needs.Difference(labels); v.Len() > 0 {
		reasons = append(reasons, fmt.Sprintf(
			msgMissingLabels, strings.Join(v.UnsortedList(), ", "),
		))
	}

	if len(cfg.MissingLabelsForMerge) > 0 {
		missing := sets.NewString(cfg.MissingLabelsForMerge...)
		if v := missing.Intersection(labels); v.Len() > 0 {
			reasons = append(reasons, fmt.Sprintf(
				msgInvalidLabels, strings.Join(v.UnsortedList(), ", "),
			))
		}
	}

	return reasons
}

type labelLog struct {
	label string
	who   string
	t     time.Time
}

func getLatestLog(ops []sdk.OperateLog, label string, log *logrus.Entry) (labelLog, bool) {
	var t time.Time

	index := -1

	for i := range ops {
		op := &ops[i]

		if op.ActionType != sdk.ActionAddLabel || !strings.Contains(op.Content, label) {
			continue
		}

		ut, err := time.Parse(time.RFC3339, op.CreatedAt)
		if err != nil {
			log.Warnf("parse time:%s failed", op.CreatedAt)

			continue
		}

		if index < 0 || ut.After(t) {
			t = ut
			index = i
		}
	}

	if index >= 0 {
		if user := ops[index].User; user != nil && user.Login != "" {
			return labelLog{
				label: label,
				t:     t,
				who:   user.Login,
			}, true
		}
	}

	return labelLog{}, false
}

func checkLabelsLegal(labels sets.String, needs sets.String, ops []sdk.OperateLog, log *logrus.Entry) string {
	f := func(label string) string {
		v, b := getLatestLog(ops, label, log)
		if !b {
			return fmt.Sprintf("The corresponding operation log is missing. you should delete " +
				"the label and add it again by correct way")
		}

		if v.who != legalLabelsAddedBy {
			if strings.HasPrefix(v.label, "openeuler-cla/") {
				return fmt.Sprintf("%s You can't add %s by yourself, "+
					"please remove it and use /check-cla to add it", v.who, v.label)
			}

			return fmt.Sprintf("%s You can't add %s by yourself, please contact the maintainers", v.who, v.label)
		}

		return ""
	}

	v := make([]string, 0, len(labels))

	for label := range labels {
		if ok := needs.Has(label); ok || strings.HasPrefix(label, lgtmLabel) {
			if s := f(label); s != "" {
				v = append(v, fmt.Sprintf("%s: %s", label, s))
			}
		}
	}

	if n := len(v); n > 0 {
		s := "label is"

		if n > 1 {
			s = "labels are"
		}

		return fmt.Sprintf("**The following %s not ready**.\n\n%s", s, strings.Join(v, "\n\n"))
	}

	return ""
}
