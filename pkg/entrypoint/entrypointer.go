/*
Copyright 2019 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package entrypoint

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	"github.com/tektoncd/pipeline/pkg/termination"
	"go.uber.org/zap"
)

// RFC3339 with millisecond
const (
	timeFormat = "2006-01-02T15:04:05.000Z07:00"
)

// Entrypointer holds fields for running commands with redirected
// entrypoints.
type Entrypointer struct {
	// Entrypoint is the original specified entrypoint, if any.
	Entrypoint string
	// Args are the original specified args, if any.
	Args []string
	// WaitFiles is the set of files to wait for. If empty, execution
	// begins immediately.
	WaitFiles []string
	// WaitFileContent indicates the WaitFile should have non-zero size
	// before continuing with execution.
	WaitFileContent bool
	// PostFile is the file to write when complete. If not specified, no
	// file is written.
	PostFile string

	// Termination path is the path of a file to write the starting time of this endpopint
	TerminationPath string

	// Waiter encapsulates waiting for files to exist.
	Waiter Waiter
	// Runner encapsulates running commands.
	Runner Runner
	// PostWriter encapsulates writing files when complete.
	PostWriter PostWriter

	// Results is the set of files that might contain task results
	Results []string
	// Timeout is an optional user-specified duration within which the Step must complete
	Timeout *time.Duration
}

// Waiter encapsulates waiting for files to exist.
type Waiter interface {
	// Wait blocks until the specified file exists.
	Wait(file string, expectContent bool) error
}

// Runner encapsulates running commands.
type Runner interface {
	Run(ctx context.Context, args ...string) error
}

// PostWriter encapsulates writing a file when complete.
type PostWriter interface {
	// Write writes to the path when complete.
	Write(file string)
}

// Go optionally waits for a file, runs the command, and writes a
// post file.
func (e Entrypointer) Go() error {
	prod, _ := zap.NewProduction()
	logger := prod.Sugar()

	output := []v1beta1.PipelineResourceResult{}
	defer func() {
		if wErr := termination.WriteMessage(e.TerminationPath, output); wErr != nil {
			logger.Fatalf("Error while writing message: %s", wErr)
		}
		_ = logger.Sync()
	}()

	for _, f := range e.WaitFiles {
		if err := e.Waiter.Wait(f, e.WaitFileContent); err != nil {
			// An error happened while waiting, so we bail
			// *but* we write postfile to make next steps bail too.
			e.WritePostFile(e.PostFile, err)
			output = append(output, v1beta1.PipelineResourceResult{
				Key:        "StartedAt",
				Value:      time.Now().Format(timeFormat),
				ResultType: v1beta1.InternalTektonResultType,
			})
			return err
		}
	}

	if e.Entrypoint != "" {
		e.Args = append([]string{e.Entrypoint}, e.Args...)
	}

	output = append(output, v1beta1.PipelineResourceResult{
		Key:        "StartedAt",
		Value:      time.Now().Format(timeFormat),
		ResultType: v1beta1.InternalTektonResultType,
	})

	ctx := context.Background()
	client, err := workloadapi.New(ctx, workloadapi.WithAddr("unix:///run/spire/sockets/agent.sock"))
	if err != nil {
		return err
	}
	jwt, err := client.FetchJWTSVID(ctx, jwtsvid.Params{
		Audience: "sigstore",
	})
	if err != nil {
		return err
	}
	fmt.Println(jwt)

	if e.Timeout != nil && *e.Timeout < time.Duration(0) {
		err = fmt.Errorf("negative timeout specified")
	}

	if err == nil {
		ctx := context.Background()
		var cancel context.CancelFunc
		if e.Timeout != nil && *e.Timeout != time.Duration(0) {
			ctx, cancel = context.WithTimeout(ctx, *e.Timeout)
			defer cancel()
		}
		err = e.Runner.Run(ctx, e.Args...)
		if err == context.DeadlineExceeded {
			output = append(output, v1beta1.PipelineResourceResult{
				Key:        "Reason",
				Value:      "TimeoutExceeded",
				ResultType: v1beta1.InternalTektonResultType,
			})
		}
	}

	// Write the post file *no matter what*
	e.WritePostFile(e.PostFile, err)

	// strings.Split(..) with an empty string returns an array that contains one element, an empty string.
	// This creates an error when trying to open the result folder as a file.
	if len(e.Results) >= 1 && e.Results[0] != "" {
		if err := e.readResultsFromDisk(client); err != nil {
			logger.Fatalf("Error while handling results: %s", err)
		}
	}

	return err
}

func Sign(results []v1beta1.PipelineResourceResult, client *workloadapi.Client) ([]v1beta1.PipelineResourceResult, error) {
	xsvid, err := client.FetchX509SVID(context.Background())
	if err != nil {
		return nil, err
	}
	output := []v1beta1.PipelineResourceResult{}
	if len(results) > 1 {
		p := pem.EncodeToMemory(&pem.Block{
			Bytes: xsvid.Certificates[0].Raw,
			Type:  "CERTIFICATE",
		})
		output = append(output, v1beta1.PipelineResourceResult{
			Key:        "SVID",
			Value:      string(p),
			ResultType: v1beta1.TaskRunResultType,
		})
	}
	for _, r := range results {
		dgst := sha256.Sum256([]byte(r.Value))
		s, err := xsvid.PrivateKey.Sign(rand.Reader, dgst[:], crypto.SHA256)
		if err != nil {
			return nil, err
		}
		output = append(output, v1beta1.PipelineResourceResult{
			Key:        r.Key + ".sig",
			Value:      base64.StdEncoding.EncodeToString(s),
			ResultType: v1beta1.TaskRunResultType,
		})
	}
	return output, nil
}

func (e Entrypointer) readResultsFromDisk(client *workloadapi.Client) error {

	output := []v1beta1.PipelineResourceResult{}
	for _, resultFile := range e.Results {
		if resultFile == "" {
			continue
		}
		fileContents, err := ioutil.ReadFile(filepath.Join(pipeline.DefaultResultPath, resultFile))
		if os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		// if the file doesn't exist, ignore it
		output = append(output, v1beta1.PipelineResourceResult{
			Key:        resultFile,
			Value:      string(fileContents),
			ResultType: v1beta1.TaskRunResultType,
		})
	}
	signed, err := Sign(output, client)
	if err != nil {
		return err
	}
	output = append(output, signed...)
	// push output to termination path
	if len(output) != 0 {
		if err := termination.WriteMessage(e.TerminationPath, output); err != nil {
			return err
		}
	}
	return nil
}

// WritePostFile write the postfile
func (e Entrypointer) WritePostFile(postFile string, err error) {
	if err != nil && postFile != "" {
		postFile = fmt.Sprintf("%s.err", postFile)
	}
	if postFile != "" {
		e.PostWriter.Write(postFile)
	}
}
