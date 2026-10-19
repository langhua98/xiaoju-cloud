package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// 以前快捷指令把文件名原样放在请求头 X-File-Name 里，iOS 的请求头放不了中文：遇到第一个非 ASCII 字符就把后面全丢了，
// 扩展名也一起丢了，服务端再按内容补上扩展名。“video_啦啦啦#扭一扭….mov”存成了“video_.mov”，“@奶茶文.mp4”存成了“@.mp4”
// （同一个文件夹里重了就是“@ (3).mp4”）。现在快捷指令先做 URL 编码；已经存成截断名字的，在上传计划（手机发来的清单
// 在请求体里，名字是完整的）和上传时认出来，改回真名。

// cutName 是 iOS 截断后的样子：第一个非 ASCII 字符之前的部分。没有非 ASCII 字符时原样返回。
func cutName(name string) string {
	for i, c := range name {
		if c > 0x7e {
			return name[:i]
		}
	}
	return name
}

// wasCut 报告真名 real（不带扩展名、不分大小写）被 iOS 截断后是不是 stored（同样处理过）：只在真名里有非 ASCII 字符、
// 截断后还剩东西时才算（开头就是中文的名字截完是空的，当时上传会因为没有文件名被拒绝，不会存下来）。
func wasCut(real, stored string) bool {
	cut := cutName(real)
	return cut != real && cut != "" && cut == stored
}

// restoreCutName 把截断名字的文件改回真名 name。真名已经被别的文件占了时不改，照样当作同一个文件。
func (s *Server) restoreCutName(ctx context.Context, id uuid.UUID, name string) (bool, error) {
	err := s.svc.Store().RenameFile(ctx, s.userID, id, name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, meta.ErrConflict):
		return false, nil
	default:
		return false, err
	}
}

// restoreCutNamesInPlan 在上传计划里处理没对上的名字：网盘里有被截断成这个样子的文件、大小（拍摄日期）也对得上，
// 就是同一个文件，改回真名、当作传过。一个文件只能对一个名字：两个名字截断后一样、大小也分不出来的，都不动（照常上传，
// 服务端按内容认出来）。返回改回了几个。
func (s *Server) restoreCutNamesInPlan(ctx context.Context, names []string, keys []string, checks []planCheck,
	stored map[string][]meta.NameSize, matched []bool) int {
	type claim struct {
		f    meta.NameSize
		idxs []int
	}
	claims := map[uuid.UUID]*claim{}
	var order []uuid.UUID
	for i := range names {
		if matched[i] {
			continue
		}
		cut := cutName(keys[i])
		if cut == keys[i] || cut == "" {
			continue
		}
		var found []meta.NameSize
		for _, f := range stored[cut] {
			if ok, _ := checks[i].dateOK(f, s.loc); !f.Alias && checks[i].sizeOK(f) && ok {
				found = append(found, f)
			}
		}
		if len(found) != 1 {
			continue
		}
		c := claims[found[0].ID]
		if c == nil {
			c = &claim{f: found[0]}
			claims[found[0].ID] = c
			order = append(order, found[0].ID)
		}
		c.idxs = append(c.idxs, i)
	}
	restored := 0
	for _, id := range order {
		c := claims[id]
		if len(c.idxs) != 1 {
			continue
		}
		i := c.idxs[0]
		name := cleanName(names[i])
		if !isMediaExt(filepath.Ext(name)) { // 清单里的名字不带扩展名：用存着的那个（当时按内容补的）
			name += filepath.Ext(c.f.Name)
		}
		renamed, err := s.restoreCutName(ctx, id, name)
		if err != nil {
			s.log.Warn("ingest plan: could not restore a cut file name", "err", err)
			continue
		}
		matched[i] = true
		if renamed {
			restored++
		}
	}
	return restored
}

// cutBefore 找文件夹里大小完全一样、名字是 name 被 iOS 截断后的样子的文件（上传时用，见 uploadedBefore），改回真名。
func (s *Server) cutBefore(ctx context.Context, same []*meta.File, name string) (*meta.File, error) {
	real := strings.ToLower(strings.TrimSuffix(stripCopySuffix(name), filepath.Ext(name)))
	for _, f := range same {
		got := strings.ToLower(stripCopySuffix(f.Name))
		got = strings.TrimSuffix(got, filepath.Ext(got))
		if !wasCut(real, got) {
			continue
		}
		renamed, err := s.restoreCutName(ctx, f.ID, name)
		if err != nil {
			return nil, err
		}
		if renamed {
			s.log.Info("restored a file name that iOS had cut", "size", f.Size)
			f.Name = name
		}
		return f, nil
	}
	return nil, nil
}
