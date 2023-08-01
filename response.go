package goyave

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"

	"gorm.io/gorm"
	errorutil "goyave.dev/goyave/v5/util/errors"
	"goyave.dev/goyave/v5/util/fsutil"
)

// TODO document Render, RenderHTML, Redirect, (more?) are not available

var (
	// ErrNotHijackable returned by response.Hijack() if the underlying
	// http.ResponseWriter doesn't implement http.Hijacker. This can
	// happen with HTTP/2 connections.
	ErrNotHijackable = errors.New("Underlying http.ResponseWriter doesn't implement http.Hijacker")
)

// PreWriter is a writter that needs to alter the response headers or status
// before they are written.
// If implemented, PreWrite will be called right before the Write operation.
type PreWriter interface {
	PreWrite(b []byte)
}

// Response implementation wrapping `http.ResponseWriter`. Writing an HTTP response without
// using it is incorrect. This acts as a proxy to one or many `io.Writer` chained, with the original
// `http.ResponseWriter` always last.
type Response struct {
	writer         io.Writer
	responseWriter http.ResponseWriter
	server         *Server
	request        *Request
	status         int

	// Used to check if controller didn't write anything so
	// core can write default 204 No Content.
	// See RFC 7231, 6.3.5
	empty       bool
	wroteHeader bool
	hijacked    bool
}

// NewResponse create a new Response using the given `http.ResponseWriter` and request.
func NewResponse(server *Server, request *Request, writer http.ResponseWriter) *Response {
	return &Response{
		server:         server,
		request:        request,
		responseWriter: writer,
		writer:         writer,
		empty:          true,
		status:         0,
		wroteHeader:    false,
	}
}

// --------------------------------------
// PreWriter implementation

// PreWrite writes the response header after calling PreWrite on the
// child writer if it implements PreWriter.
func (r *Response) PreWrite(b []byte) {
	r.empty = false
	if pr, ok := r.writer.(PreWriter); ok {
		pr.PreWrite(b)
	}
	if !r.wroteHeader {
		if r.status == 0 {
			r.status = http.StatusOK
		}
		r.WriteHeader(r.status)
	}
}

// --------------------------------------
// http.ResponseWriter implementation

// Write writes the data as a response.
// See http.ResponseWriter.Write
func (r *Response) Write(data []byte) (int, error) {
	r.PreWrite(data)
	n, err := r.writer.Write(data)
	if err != nil {
		err = errorutil.New(err)
	}
	return n, err
}

// WriteHeader sends an HTTP response header with the provided
// status code.
// Prefer using "Status()" method instead.
// Calling this method a second time will have no effect.
func (r *Response) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
		r.responseWriter.WriteHeader(status)
	}
}

// Header returns the header map that will be sent.
func (r *Response) Header() http.Header {
	return r.responseWriter.Header()
}

// Cookie add a Set-Cookie header to the response.
// The provided cookie must have a valid Name. Invalid cookies may be
// silently dropped.
func (r *Response) Cookie(cookie *http.Cookie) {
	http.SetCookie(r.responseWriter, cookie)
}

// --------------------------------------
// http.Hijacker implementation

// Hijack implements the Hijacker.Hijack method.
// For more details, check http.Hijacker.
//
// Returns ErrNotHijackable if the underlying http.ResponseWriter doesn't
// implement http.Hijacker. This can happen with HTTP/2 connections.
//
// Middleware executed after controller handlers, as well as status handlers,
// keep working as usual after a connection has been hijacked.
// Callers should properly set the response status to ensure middleware and
// status handler execute correctly. Usually, callers of the Hijack method
// set the HTTP status to http.StatusSwitchingProtocols.
// If no status is set, the regular behavior will be kept and `204 No Content`
// will be set as the response status.
func (r *Response) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.responseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, ErrNotHijackable
	}
	c, b, e := hijacker.Hijack()
	if e == nil {
		r.hijacked = true
	} else {
		e = errorutil.New(e)
	}
	return c, b, e
}

// Hijacked returns true if the underlying connection has been successfully hijacked
// via the Hijack method.
func (r *Response) Hijacked() bool {
	return r.hijacked
}

// --------------------------------------
// Chained writers

// Writer return the current writer used to write the response.
// Note that the returned writer is not necessarily a http.ResponseWriter, as
// it can be replaced using SetWriter.
func (r *Response) Writer() io.Writer {
	return r.writer
}

// SetWriter set the writer used to write the response.
// This can be used to chain writers, for example to enable
// gzip compression, or for logging.
//
// The original http.ResponseWriter is always kept.
func (r *Response) SetWriter(writer io.Writer) {
	r.writer = writer
}

func (r *Response) close() error {
	if wr, ok := r.writer.(io.Closer); ok {
		return wr.Close()
	}
	return nil
}

// --------------------------------------
// Accessors

// GetStatus return the response code for this request or 0 if not yet set.
func (r *Response) GetStatus() int {
	return r.status
}

// IsEmpty return true if nothing has been written to the response body yet.
func (r *Response) IsEmpty() bool {
	return r.empty
}

// IsHeaderWritten return true if the response header has been written.
// Once the response header is written, you cannot change the response status
// and headers anymore.
func (r *Response) IsHeaderWritten() bool {
	return r.wroteHeader
}

// --------------------------------------
// Write methods

// Status set the response status code.
// Calling this method a second time will have no effect.
func (r *Response) Status(status int) {
	if r.status == 0 {
		r.status = status
	}
}

// JSON write json data as a response.
// Also sets the "Content-Type" header automatically.
func (r *Response) JSON(responseCode int, data interface{}) {
	r.responseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")
	r.status = responseCode
	if err := json.NewEncoder(r).Encode(data); err != nil {
		panic(errorutil.NewSkip(err, 3))
	}
}

// String write a string as a response
func (r *Response) String(responseCode int, message string) {
	r.status = responseCode
	if _, err := r.Write([]byte(message)); err != nil {
		panic(errorutil.NewSkip(err, 3))
	}
}

func (r *Response) writeFile(file string, disposition string) { // TODO handle io.FS
	if !fsutil.FileExists(file) {
		r.Status(http.StatusNotFound)
		return
	}
	r.empty = false
	r.status = http.StatusOK
	mime, size, err := fsutil.GetMIMEType(file)
	if err != nil {
		r.Error(errorutil.NewSkip(err, 4))
		return
	}
	header := r.responseWriter.Header()
	header.Set("Content-Disposition", disposition)

	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", mime)
	}

	header.Set("Content-Length", strconv.FormatInt(size, 10))

	f, _ := os.Open(file)
	// No need to check for errors, fsutil.FileExists(file) and
	// fsutil.GetMIMEType(file) already handled that.
	defer func() {
		_ = f.Close()
	}()
	if _, err := io.Copy(r, f); err != nil {
		panic(errorutil.NewSkip(err, 4))
	}
}

// File write a file as an inline element.
// Automatically detects the file MIME type and sets the "Content-Type" header accordingly.
// If the file doesn't exist, respond with status 404 Not Found.
// The given path can be relative or absolute.
//
// If you want the file to be sent as a download ("Content-Disposition: attachment"), use the "Download" function instead.
func (r *Response) File(file string) {
	r.writeFile(file, "inline")
}

// Download write a file as an attachment element.
// Automatically detects the file MIME type and sets the "Content-Type" header accordingly.
// If the file doesn't exist, respond with status 404 Not Found.
// The given path can be relative or absolute.
//
// The "fileName" parameter defines the name the client will see. In other words, it sets the header "Content-Disposition" to
// "attachment; filename="${fileName}""
//
// If you want the file to be sent as an inline element ("Content-Disposition: inline"), use the "File" function instead.
func (r *Response) Download(file string, fileName string) {
	r.writeFile(file, fmt.Sprintf("attachment; filename=\"%s\"", fileName))
}

// Error print the error in the console and return it with an error code 500 (or previously defined
// status code using `response.Status()`).
// If debugging is enabled in the config, the error is also written in the response
// and the stacktrace is printed in the console.
// If debugging is not enabled, only the status code is set, which means you can still
// write to the response, or use your error status handler.
func (r *Response) Error(err any) {
	e := errorutil.NewSkip(err, 3) // Skipped: runtime.Callers, NewSkip, this func
	r.server.ErrLogger.Println(e.String())
	r.error(e)
}

func (r *Response) error(err any) {
	e := errorutil.NewSkip(err, 3)  // Skipped: runtime.Callers, NewSkip, this func
	r.request.Extra[ExtraError] = e // TODO should errors not be in Extras? Should they be guaranteed to be errors.Error?

	if r.server.Config().GetBool("app.debug") && r.IsEmpty() && !r.Hijacked() {
		status := http.StatusInternalServerError
		if r.status != 0 {
			status = r.status
		}
		r.JSON(status, map[string]any{"error": e})
		return
	}

	// Don't set r.empty to false to let error status handler process the error
	r.Status(http.StatusInternalServerError)
}

// WriteDBError takes a database query result and checks if any error has occurred.
//
// Automatically writes HTTP status code 404 Not Found if the error is a "Not found" error.
// Calls `Response.Error()` if there is another type of error.
//
// Returns true if there is an error. You can then safely `return` in you controller.
//
//	func (ctrl *ProductController) Show(response *goyave.Response, request *goyave.Request) {
//	    product := model.Product{}
//	    result := ctrl.DB().First(&product, request.Params["id"])
//	    if response.WriteDBError(result.Error) {
//	        return
//	    }
//	    response.JSON(http.StatusOK, product)
//	}
func (r *Response) WriteDBError(err error) bool {
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			r.Status(http.StatusNotFound)
		} else {
			r.Error(errorutil.NewSkip(err, 3))
		}
		return true
	}
	return false
}