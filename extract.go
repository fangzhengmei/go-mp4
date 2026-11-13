package mp4

import (
	"errors"
	"io"
)

type BoxInfoWithPayload struct {
	Info    BoxInfo
	Payload IBox
}

func ExtractBoxWithPayload(r io.ReadSeeker, parent *BoxInfo, path BoxPath) ([]*BoxInfoWithPayload, error) {
	return ExtractBoxesWithPayload(r, parent, []BoxPath{path})
}

func ExtractBoxesWithPayload(r io.ReadSeeker, parent *BoxInfo, paths []BoxPath) ([]*BoxInfoWithPayload, error) {
	if len(paths) == 0 {
		return nil, nil
	}

	for i := range paths {
		if len(paths[i]) == 0 {
			return nil, errors.New("box path must not be empty")
		}
	}

	bs := make([]*BoxInfoWithPayload, 0, 8)
	handler := func(handle *ReadHandle) (interface{}, error) {
		path := handle.Path
		if parent != nil {
			path = path[1:]
		}
		if handle.BoxInfo.Type == BoxTypeAny() {
			return nil, nil
		}

		fm, m := matchPath(paths, path)
		if m {
			box, _, err := handle.ReadPayload()
			if err != nil {
				return nil, err
			}
			bs = append(bs, &BoxInfoWithPayload{
				Info:    handle.BoxInfo,
				Payload: box,
			})
		}

		if fm {
			if _, err := handle.Expand(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	if parent != nil {
		_, err := ReadBoxStructureFromInternal(r, parent, handler)
		return bs, err
	}
	_, err := ReadBoxStructure(r, handler)
	return bs, err
}

func ExtractBox(r io.ReadSeeker, parent *BoxInfo, path BoxPath) ([]*BoxInfo, error) {
	return ExtractBoxes(r, parent, []BoxPath{path})
}

func ExtractBoxes(r io.ReadSeeker, parent *BoxInfo, paths []BoxPath) ([]*BoxInfo, error) {
	if len(paths) == 0 {
		return nil, nil
	}

	for i := range paths {
		if len(paths[i]) == 0 {
			return nil, errors.New("box path must not be empty")
		}
	}

	boxes := make([]*BoxInfo, 0, 8)

	handler := func(handle *ReadHandle) (interface{}, error) {
		path := handle.Path
		if parent != nil {
			path = path[1:]
		}
		if handle.BoxInfo.Type == BoxTypeAny() {
			return nil, nil
		}
		fm, m := matchPath(paths, path)
		if m {
			boxes = append(boxes, &handle.BoxInfo)
		}

		if fm {
			if _, err := handle.Expand(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	if parent != nil {
		_, err := ReadBoxStructureFromInternal(r, parent, handler)
		return boxes, err
	}
	_, err := ReadBoxStructure(r, handler)
	return boxes, err
}

func matchPath(paths []BoxPath, path BoxPath) (forwardMatch bool, match bool) {
	for i := range paths {
		fm, m := path.compareWith(paths[i])
		forwardMatch = forwardMatch || fm
		match = match || m
	}
	return
}
