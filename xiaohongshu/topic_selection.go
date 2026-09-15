package xiaohongshu

import (
	"errors"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

var errTopicUnbound = errors.New("topic_unbound")

func exactTopicIndex(labels []string, tag string) (int, error) {
	found := -1
	for i, label := range labels {
		// One leading hash may be the widget's display decoration. The frozen tag
		// itself is never trimmed, case-folded or stripped.
		if label == tag || label == "#"+tag {
			if found >= 0 {
				return -1, errTopicUnbound
			}
			found = i
		}
	}
	if found < 0 {
		return -1, errTopicUnbound
	}
	return found, nil
}
func selectExactTopic(page *rod.Page, tag string) error {
	items, err := page.Elements("#creator-editor-topic-container .item")
	if err != nil {
		return errTopicUnbound
	}
	var visible []*rod.Element
	var labels []string
	for _, item := range items {
		shown, err := item.Visible()
		if err != nil {
			return errTopicUnbound
		}
		if !shown {
			continue
		}
		label, err := item.Text()
		if err != nil {
			return errTopicUnbound
		}
		visible = append(visible, item)
		labels = append(labels, label)
	}
	index, err := exactTopicIndex(labels, tag)
	if err != nil {
		return err
	}
	if visible[index].Click(proto.InputMouseButtonLeft, 1) != nil {
		return errTopicUnbound
	}
	return nil
}
