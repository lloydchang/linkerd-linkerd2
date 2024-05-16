package cmd

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/linkerd/linkerd2/pkg/inject"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	yamlDecoder "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

type resourceTransformer interface {
	transform([]byte) ([]byte, []inject.Report, error)
	generateReport([]inject.Report, io.Writer)
}

// Returns the integer representation of os.Exit code; 0 on success and 1 on failure.
func transformInput(inputs []io.Reader, errWriter, outWriter io.Writer, rt resourceTransformer, format string) int {
	postInjectBuf := &bytes.Buffer{}
	reportBuf := &bytes.Buffer{}

	for _, input := range inputs {
		errs := processYAML(input, postInjectBuf, reportBuf, rt, format)
		if len(errs) > 0 {
			fmt.Fprintf(errWriter, "Error transforming resources:\n%v", concatErrors(errs, "\n"))
			return 1
		}

		_, err := io.Copy(outWriter, postInjectBuf)

		// print error report after yaml output, for better visibility
		io.Copy(errWriter, reportBuf)

		if err != nil {
			fmt.Fprintf(errWriter, "Error printing YAML: %v\n", err)
			return 1
		}
	}
	return 0
}

// processYAML takes an input stream of YAML, outputting injected/uninjected YAML to out.
func processYAML(in io.Reader, out io.Writer, report io.Writer, rt resourceTransformer, format string) []error {
	var reader yamlDecoder.Reader
	buffer, _, isJSON := guessJSONStream(in, 4096)
	if isJSON {
		// We assume that json documents will be separated by newlines.
		reader = &lineReader{reader: buffer}

	} else {
		reader = yamlDecoder.NewYAMLReader(buffer)
	}

	reports := []inject.Report{}

	errs := []error{}

	// Iterate over all YAML objects in the input
	for {
		// Read a single YAML object
		bytes, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return []error{err}
		}

		var result []byte
		var irs []inject.Report

		isList, err := kindIsList(bytes)
		if err != nil {
			return []error{err}
		}
		if isList {
			result, irs, err = processList(bytes, rt)
		} else {
			result, irs, err = rt.transform(bytes)
		}
		if err != nil {
			errs = append(errs, err)
		}
		reports = append(reports, irs...)

		// If the format is set to json, we need to convert the yaml to json
		if format == "json" {
			result, err = yaml.YAMLToJSON(result)
			if err != nil {
				errs = append(errs, err)
			}
		} else if format == "yaml" {
			// result is already in yaml format: noop.
		} else {
			errs = append(errs, fmt.Errorf("unsupported format %s", format))
		}

		if len(errs) == 0 {
			out.Write(result)
			if format == "yaml" {
				out.Write([]byte("---\n"))
			}
			if format == "json" {
				out.Write([]byte("\n"))
			}
		}
	}

	rt.generateReport(reports, report)

	return errs
}

func kindIsList(bytes []byte) (bool, error) {
	var meta metav1.TypeMeta
	if err := yaml.Unmarshal(bytes, &meta); err != nil {
		return false, err
	}
	return meta.Kind == "List", nil
}

func processList(bytes []byte, rt resourceTransformer) ([]byte, []inject.Report, error) {
	var sourceList corev1.List
	if err := yaml.Unmarshal(bytes, &sourceList); err != nil {
		return nil, nil, err
	}

	reports := []inject.Report{}
	items := []runtime.RawExtension{}

	for _, item := range sourceList.Items {
		result, irs, err := rt.transform(item.Raw)
		if err != nil {
			return nil, nil, err
		}

		// At this point, we have yaml. The kubernetes internal representation is
		// json. Because we're building a list from RawExtensions, the yaml needs
		// to be converted to json.
		injected, err := yaml.YAMLToJSON(result)
		if err != nil {
			return nil, nil, err
		}

		items = append(items, runtime.RawExtension{Raw: injected})
		reports = append(reports, irs...)
	}

	sourceList.Items = items
	result, err := yaml.Marshal(sourceList)
	if err != nil {
		return nil, nil, err
	}
	return result, reports, nil
}

// Read all the resource files found in path into a slice of readers.
// path can be either a file, directory or stdin.
func read(path string) ([]io.Reader, error) {
	if path == "-" {
		return []io.Reader{os.Stdin}, nil
	}

	if url, ok := toURL(path); ok {
		if strings.ToLower(url.Scheme) != "https" {
			return nil, fmt.Errorf("only HTTPS URLs are allowed")
		}
		resp, err := http.Get(url.String())
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unable to read URL %q, server reported %s, status code=%d", path, resp.Status, resp.StatusCode)
		}

		// Save to a buffer, so that response can be closed here
		buf := new(bytes.Buffer)
		_, err = buf.ReadFrom(resp.Body)
		if err != nil {
			return nil, err
		}

		return []io.Reader{buf}, nil
	}

	return walk(path)
}

// checks if the given string is a valid URL
func toURL(path string) (*url.URL, bool) {
	u, err := url.ParseRequestURI(path)
	if err == nil && u.Host != "" && u.Scheme != "" {
		return u, true
	}

	return nil, false
}

// walk walks the file tree rooted at path. path may be a file or a directory.
// Creates a reader for each file found.
func walk(path string) ([]io.Reader, error) {
	p := filepath.Clean(path)
	stat, err := os.Stat(p)
	if err != nil {
		return nil, err
	}

	if !stat.IsDir() {
		file, err := os.Open(p)
		if err != nil {
			return nil, err
		}

		return []io.Reader{file}, nil
	}

	var in []io.Reader
	werr := filepath.Walk(p, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		file, err := os.Open(filepath.Clean(path))
		if err != nil {
			return err
		}

		in = append(in, file)
		return nil
	})

	if werr != nil {
		return nil, werr
	}

	return in, nil
}

// a helper function to concatenate the items in a []error
// into a single error
func concatErrors(errs []error, delimiter string) error {
	message, errs := errs[0].Error(), errs[1:] // pop the first element of the errs
	// this is done so that the first error message is not prefixed by the delimiter

	for _, err := range errs {
		message = fmt.Sprintf("%s%s%s", message, delimiter, err.Error())
	}
	return errors.New(message)
}

// We copy lineReader, guessJSONStream, hasJSONPrefix, jsonPrefix, and hasPrefix
// from https://github.com/kubernetes/apimachinery/blob/1da46c3f5a5b4a0cc756cb6050df0cf6f06b64c2/pkg/util/yaml/decoder.go#L347
// because lineReader does not have a public constructor and so that we can
// refine the return type of guessJSONStream from *io.Reader to *bufio.Reader.
type lineReader struct {
	reader *bufio.Reader
}

// Read returns a single line (with '\n' ended) from the underlying reader.
// An error is returned iff there is an error with the underlying reader.
func (r *lineReader) Read() ([]byte, error) {
	var (
		isPrefix bool  = true
		err      error = nil
		line     []byte
		buffer   bytes.Buffer
	)

	for isPrefix && err == nil {
		line, isPrefix, err = r.reader.ReadLine()
		buffer.Write(line)
	}
	buffer.WriteByte('\n')
	return buffer.Bytes(), err
}

// guessJSONStream scans the provided reader up to size, looking
// for an open brace indicating this is JSON. It will return the
// bufio.Reader it creates for the consumer.
func guessJSONStream(r io.Reader, size int) (*bufio.Reader, []byte, bool) {
	buffer := bufio.NewReaderSize(r, size)
	b, _ := buffer.Peek(size)
	return buffer, b, hasJSONPrefix(b)
}

var jsonPrefix = []byte("{")

// hasJSONPrefix returns true if the provided buffer appears to start with
// a JSON open brace.
func hasJSONPrefix(buf []byte) bool {
	return hasPrefix(buf, jsonPrefix)
}

// Return true if the first non-whitespace bytes in buf is
// prefix.
func hasPrefix(buf []byte, prefix []byte) bool {
	trim := bytes.TrimLeftFunc(buf, unicode.IsSpace)
	return bytes.HasPrefix(trim, prefix)
}
