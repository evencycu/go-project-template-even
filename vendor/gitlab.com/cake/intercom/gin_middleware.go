package intercom

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"gitlab.com/cake/goctx"
	"gitlab.com/cake/gopkg"
	"gitlab.com/cake/gotrace/v2"
	"gitlab.com/cake/m800log"
)

var (
	dunno     = []byte("???")
	centerDot = []byte("·")
	dot       = []byte(".")
	slash     = []byte("/")
)

var (
	slowReqDuration = 5 * time.Second
)

var (
	proxyMap = sync.Map{}
)

const (
	TraceTagGinError = "gin.error"
	TraceTagNs       = "ns"
	TraceTagForward  = "forward.host"
	TraceTagFromNs   = "from.ns"

	crossSpanName          = "cross region forward"
	proxyScheme            = "http"
	proxyHeaderForwardHost = "X-Forward-Host"
	proxyHeaderOriginHost  = "X-Origin-Host"
)

const (
	LogHideWildcardName = "*"
)

type LogHideOption struct {
	HandlerName   string
	RequestHider  func(b []byte) []byte
	ResponseHider func(httpStatus int, b []byte) []byte
}

type bodyLogWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

func (w bodyLogWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

func SetSlowReqThreshold(t time.Duration) {
	slowReqDuration = t
}

// M800Recovery does the recover for gin handler with M800 response
func M800Recovery(panicCode int) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			handlerName := "M800Recovery"
			if err := recover(); err != nil {
				// Check for a broken connection, as it is not really a condition that warrants a panic stack trace.
				var brokenPipe bool
				var errSyscall *os.SyscallError
				if ne, ok := err.(error); ok && errors.As(ne, &errSyscall) &&
					(strings.Contains(strings.ToLower(errSyscall.Error()), "broken pipe") ||
						strings.Contains(strings.ToLower(errSyscall.Error()), "connection reset by peer")) {
					brokenPipe = true
				}

				stack := stack(3)
				panicStr := fmt.Sprintf("[%s] recovered at: %s, panic err:\n%s,\nstack:\n%s",
					handlerName, timeFormat(time.Now()), err, stack)

				ctx := GetContextFromGin(c)
				ctx.Set(goctx.LogKeyErrorCode, panicCode)

				// If the connection is dead, we can't write a status to it.
				if brokenPipe {
					label := prometheus.Labels{labelDownstream: downstreamName(c)}
					brokenPipeCounts.With(label).Inc()
					c.Error(err.(error)) // nolint: errcheck
					c.Abort()
					return
				}

				m800log.Error(ctx, panicStr)
				GinErrorCodeMsg(c, panicCode, fmt.Sprintf("%s", err))
			}
		}()
		c.Next()
	}
}

func downstreamName(c *gin.Context) string {
	ctx := GetContextFromGin(c)
	if caller, ok := ctx.GetString(goctx.HTTPHeaderInternalCaller); ok {
		return caller
	}
	if strings.Contains(c.GetHeader("User-Agent"), "Go-http-client") {
		return "golang-client"
	}
	return "client"
}

// stack returns a nicely formatted stack frame, skipping skip frames.
func stack(skip int) []byte {
	buf := new(bytes.Buffer) // the returned data
	// As we loop, we open files and read them. These variables record the currently
	// loaded file.
	var lines [][]byte
	var lastFile string
	for i := skip; ; i++ { // Skip the expected number of frames
		pc, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}
		// Print this much at least.  If we can't find the source, it won't show.
		fmt.Fprintf(buf, "%s:%d (0x%x)\n", file, line, pc)
		if file != lastFile {
			data, err := ioutil.ReadFile(file)
			if err != nil {
				continue
			}
			lines = bytes.Split(data, []byte{'\n'})
			lastFile = file
		}
		fmt.Fprintf(buf, "\t%s: %s\n", function(pc), source(lines, line))
	}
	return buf.Bytes()
}

// source returns a space-trimmed slice of the n'th line.
func source(lines [][]byte, n int) []byte {
	n-- // in stack trace, lines are 1-indexed but our array is 0-indexed
	if n < 0 || n >= len(lines) {
		return dunno
	}
	return bytes.TrimSpace(lines[n])
}

// function returns, if possible, the name of the function containing the PC.
func function(pc uintptr) []byte {
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return dunno
	}
	name := []byte(fn.Name())
	// The name includes the path name to the package, which is unnecessary
	// since the file name is already included.  Plus, it has center dots.
	// That is, we see
	//	runtime/debug.*T·ptrmethod
	// and want
	//	*T.ptrmethod
	// Also the package path might contains dot (e.g. code.google.com/...),
	// so first eliminate the path prefix
	if lastSlash := bytes.LastIndex(name, slash); lastSlash >= 0 {
		name = name[lastSlash+1:]
	}
	if period := bytes.Index(name, dot); period >= 0 {
		name = name[period+1:]
	}
	name = bytes.Replace(name, centerDot, dot, -1)
	return name
}

func timeFormat(t time.Time) string {
	var timeString = t.Format(time.RFC3339Nano)
	return timeString
}

// AccessMiddleware
func AccessMiddleware(timeout time.Duration, localNamespace string, opts ...*LogHideOption) gin.HandlerFunc {
	hiderReqMap := make(map[string]func(b []byte) []byte)
	hiderRespMap := make(map[string]func(httpStatus int, b []byte) []byte)
	for _, opt := range opts {
		if opt.RequestHider != nil {
			hiderReqMap[opt.HandlerName] = opt.RequestHider
		}
		if opt.ResponseHider != nil {
			hiderRespMap[opt.HandlerName] = opt.ResponseHider
		}
	}
	return func(c *gin.Context) {
		blw := &bodyLogWriter{body: &bytes.Buffer{}, ResponseWriter: c.Writer}
		c.Writer = blw
		ctx := GetContextFromGin(c)
		ctx.Set(goctx.LogKeyHTTPMethod, c.Request.Method)
		ctx.Set(goctx.LogKeyURI, c.Request.URL.RequestURI())
		// init if no cid
		_, ok := ctx.GetString(goctx.LogKeyCID)
		if !ok {
			cid, _ := ctx.GetCID()
			c.Request.Header.Set(goctx.HTTPHeaderCID, cid)
		}

		start := time.Now().UTC()
		defer m800log.Access(ctx, start)

		cancel := ctx.SetTimeout(timeout)
		defer cancel()
		handlerName := c.HandlerName()
		ctx.Set(LogEntryHandlerName, handlerName)
		sp, needFinish := gotrace.CreateSpan(ctx, handlerName)
		if needFinish {
			defer sp.Finish()
		}
		sp.SetTag(TraceTagNs, localNamespace)
		var httpBody []byte
		if c.Request.Body != nil {
			var err gopkg.CodeError
			httpBody, err = ReadFromReadCloser(c.Request.Body)
			if err != nil {
				GinError(c, err)
				return
			}
			// support common http req usage
			c.Request.Body = ioutil.NopCloser(bytes.NewReader(httpBody))
			c.Set(KeyBody, httpBody)
		}

		c.Next()
		select {
		case <-ctx.Done():
			m800log.Debug(ctx, "ctx done case")
		default:
			// common case
		}
		elapsed := time.Since(start)
		if elapsed > slowReqDuration {
			ext.SamplingPriority.Set(sp, uint16(1))
			ctx.Set("slow", elapsed)
		}
		if elapsed > timeout {
			m800log.Errorf(ctx, "api timeout, timeout setting: %s, elapsed: %s", timeout, elapsed)
		} else if elapsed > slowReqDuration {
			m800log.Warnf(ctx, "api slow, slow setting: %s, elapsed: %s", slowReqDuration, elapsed)
		}

		if traceErrCode := c.GetInt(goctx.LogKeyErrorCode); traceErrCode != 0 {
			ext.SamplingPriority.Set(sp, uint16(1))
			ext.Error.Set(sp, true)
			sp.SetTag(TraceTagGinErrorCode, traceErrCode)
			ctx.Set(goctx.LogKeyErrorCode, traceErrCode)
			dumpLogHandle(ctx, c, handlerName, httpBody, blw, elapsed, ErrorTraceLevel, hiderReqMap, hiderRespMap)
			return
		}
		if m800log.GetLogger().Level >= logrus.DebugLevel {
			dumpLogHandle(ctx, c, handlerName, httpBody, blw, elapsed, logrus.DebugLevel, hiderReqMap, hiderRespMap)
			return
		}
	}
}

func dumpLogHandle(ctx goctx.Context, c *gin.Context, handlerName string, httpBody []byte, blw *bodyLogWriter, elapsed time.Duration, ErrorTraceLevel logrus.Level, hiderReqMap map[string]func(b []byte) []byte, hiderRespMap map[string]func(httpStatus int, b []byte) []byte) {
	strs := strings.Split(handlerName, ".")
	logHandlerName := strs[len(strs)-1]
	logReqBody, logRespBody := httpBody, blw.body.Bytes()
	if reqHider := hiderReqMap[logHandlerName]; reqHider != nil {
		logReqBody = reqHider(logReqBody)
	} else if reqHider := hiderReqMap[LogHideWildcardName]; reqHider != nil {
		logReqBody = reqHider(logReqBody)
	}
	if respHider := hiderRespMap[logHandlerName]; respHider != nil {
		logRespBody = respHider(c.Writer.Status(), logRespBody)
	} else if respHider := hiderRespMap[LogHideWildcardName]; respHider != nil {
		logRespBody = respHider(c.Writer.Status(), logRespBody)
	}
	dumpRequestGivenBody(ctx, ErrorTraceLevel, c.Request, logReqBody)
	m800log.Logf(ctx, ErrorTraceLevel, "API Response %d: duration: %s body: %s", c.Writer.Status(), elapsed, logRespBody)
}

func newProxy(ctx goctx.Context, forwardedHost string, timeout time.Duration, proxyErrorCode int) *httputil.ReverseProxy {
	v, ok := proxyMap.Load(forwardedHost)
	if ok {
		return v.(*httputil.ReverseProxy)
	}
	director := func(req *http.Request) {
		req.Header.Add(proxyHeaderForwardHost, forwardedHost)
		req.Header.Add(proxyHeaderOriginHost, req.Host)
		req.URL.Scheme = proxyScheme
		req.URL.Host = forwardedHost
	}
	proxy := &httputil.ReverseProxy{
		Director: director,
		Transport: &http.Transport{
			ResponseHeaderTimeout: timeout,
		},
		ErrorHandler: func(resp http.ResponseWriter, req *http.Request, err error) {
			if err != nil {
				response := Response{}
				response.Code = proxyErrorCode
				response.Message = "cross region forward error: " + err.Error()
				byteResp, err := json.Marshal(response)
				if err != nil {
					byteResp = []byte(fmt.Sprintf(`{"code":%d,"message":"cross region forward error"}`, proxyErrorCode))
				}
				resp.Header().Set(HeaderContentType, HeaderJSON)
				resp.WriteHeader(http.StatusBadGateway)
				_, errWrite := resp.Write(byteResp)
				if errWrite != nil {
					m800log.Info(ctx, "proxy response write error: ", errWrite)
				}
			}
		},
	}
	proxyMap.Store(forwardedHost, proxy)
	return proxy
}

func CrossRegionNamespaceMiddleware(service, servicePort, localNamespace string, nsFunc func(c *gin.Context) (string, gopkg.CodeError), timeout time.Duration, proxyErrorCode int) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Edge server would carry the service home region info
		// https://issuetracking.maaii.com:9443/display/LCC5/Edge+Server+Header+Rules

		ns, err := nsFunc(c)
		if err != nil {
			errMsg := fmt.Sprintf("cross region lookup ns failed, err: %+v", err)
			GinOKError(c, gopkg.NewCodeError(proxyErrorCode, errMsg))
			c.Abort()
			return
		}

		if ns == "" {
			GinError(c, gopkg.NewCodeError(CodeEmptyServiceHome, MsgEmptyServiceHome))
			c.Abort()
			return
		}

		cid := c.GetHeader(goctx.HTTPHeaderCID)
		path := c.Request.URL.Path
		if ns == localNamespace {
			c.Next()
			return
		}
		ctx := GetContextFromGin(c)
		sp, needFinish := gotrace.CreateSpan(ctx, c.HandlerName())
		if needFinish {
			defer sp.Finish()
		}

		errInject := gotrace.InjectSpan(sp, c.Request.Header)
		if errInject != nil {
			m800log.Info(ctx, "create inject span error:", errInject)
		}
		tags := &gotrace.TagsMap{
			Header: c.Request.Header,
			Method: c.Request.Method,
		}
		forwardedHost := service + "." + ns + ":" + servicePort
		crossSp := gotrace.CreateChildOfSpan(ctx, crossSpanName)
		defer crossSp.Finish()

		gotrace.AttachHttpTags(crossSp, tags)
		crossSp.SetTag(TraceTagForward, forwardedHost)
		crossSp.SetTag(TraceTagFromNs, localNamespace)
		m800log.Debugf(ctx, "[cross region middleware] do the cross forward to :%s, cid: %s, path: %s", forwardedHost, cid, path)

		proxy := newProxy(ctx, forwardedHost, timeout, proxyErrorCode)
		logWriter := m800log.GetLogger().WriterLevel(logrus.InfoLevel)
		proxy.ErrorLog = log.New(logWriter, "", 0)
		defer logWriter.Close()
		defer func() {
			if err := recover(); err != nil {
				// It could because downsteam or upstream disconnets. See https://github.com/gin-gonic/gin/issues/1714
				if ne, ok := err.(error); ok && errors.Is(ne, http.ErrAbortHandler) {
					ctx.Set(goctx.LogKeyErrorCode, CodePanic)

					label := prometheus.Labels{labelDownstream: downstreamName(c), labelUpstream: service, labelUpstreamNamespace: ns}
					proxyBrokenPipeCounts.With(label).Inc()

					m800log.Debugf(ctx, "[cross region middleware] cross region abort panic: %+v", ne)
					return
				}
				panic(err)
			}
		}()
		ctx.InjectHTTPHeader(c.Request.Header)
		proxy.ServeHTTP(c.Writer, c.Request)
		c.Abort()
	}
}

// CrossRegionMiddleware
func CrossRegionMiddleware(service, servicePort, localNamespace string, timeout time.Duration, proxyErrorCode int) gin.HandlerFunc {
	nsFunc := func(c *gin.Context) (string, gopkg.CodeError) {
		return c.GetHeader(goctx.HTTPHeaderServiceHome), nil
	}

	return CrossRegionNamespaceMiddleware(service, servicePort, localNamespace, nsFunc, timeout, proxyErrorCode)
}

func BanAnonymousMiddleware(errCode gopkg.CodeError) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := c.Request.Header[http.CanonicalHeaderKey(goctx.HTTPHeaderUserAnms)]; !ok {
			c.Next()
			return
		}

		isAnms, err := strconv.ParseBool(c.GetHeader(goctx.HTTPHeaderUserAnms))
		if isAnms {
			GinError(c, errCode)
			c.Abort()
			return
		}

		if err != nil {
			GinErrorCodeMsg(c, CodeMaliciousHeader, MsgErrMaliciousHeader)
			c.Abort()
			return
		}

		c.Next()
	}
}

// utils func for hiding info in LogHideOption
func FindNextJsonStringValue(str, key string) (value string, endIndex int) {
	keyIndex := strings.Index(str, key)
	if keyIndex < 0 {
		return "", -1
	}

	if key[0] != '"' {
		key = `"` + key
	}
	if key[len(key)-1] != '"' {
		key = key + `"`
	}
	kl := len(key)

	keyNextIndex := keyIndex + kl
	nextQ1Index := strings.Index(str[keyNextIndex:], `"`)
	strNextQ1Index := keyNextIndex + nextQ1Index + 1
	nextQ2Offset := 0
	for nextQ2Offset < len(str)-strNextQ1Index {
		nextIndex := strings.Index(str[strNextQ1Index+nextQ2Offset:], `"`)
		if nextIndex < 0 {
			break
		}
		if str[strNextQ1Index+nextIndex+nextQ2Offset-1] != '\\' {
			nextQ2Offset += nextIndex
			break
		}
		nextQ2Offset += nextIndex + 1
	}
	endIndex = strNextQ1Index + nextQ2Offset
	return str[strNextQ1Index:endIndex], endIndex
}

func FindAllJsonStringValue(str, key string) (values []string) {
	for {
		value, index := FindNextJsonStringValue(str, key)
		if index < 0 {
			return
		}
		values = append(values, value)
		str = str[index:]
	}
}

// memory performance issue
func ReplaceUserTextJson(input, key string) string {
	values := FindAllJsonStringValue(input, key)
	for i := range values {
		input = strings.ReplaceAll(input, values[i], fmt.Sprintf("hidetext_len:%d", len(values[i])))
	}
	return input
}

func ReplaceUserTextHeader(input, key string) string {
	lowerStr := strings.ToLower(input)
	keyIndex := strings.Index(lowerStr, key)
	if keyIndex < 0 {
		return input
	}

	// header with ":"
	keyEndIndex := keyIndex + len(key) + 1
	newlineIndex := strings.Index(input[keyEndIndex:], "\n")
	value := input[keyEndIndex : keyEndIndex+newlineIndex]
	input = input[:keyEndIndex] + fmt.Sprintf("hidetext_len:%d", len(value)) + input[keyEndIndex+newlineIndex:]

	return input
}

// example reference to im-common/middleware.go

func HideReqB64(input []byte) []byte {
	buf := make([]byte, base64.StdEncoding.EncodedLen(len(input)))
	base64.StdEncoding.Encode(buf, input)
	return buf
}

func HideResponseB64(status int, input []byte) []byte {
	if status != http.StatusOK {
		return input
	}
	buf := make([]byte, base64.StdEncoding.EncodedLen(len(input)))
	base64.StdEncoding.Encode(buf, input)

	return buf
}
