package tos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
)

// CreateMultipartUpload create a multipart upload operation
//   objectKey: the name of object
//   options: WithContentType set Content-Type,
//     WithContentDisposition set Content-Disposition,
//     WithContentLanguage set Content-Language,
//     WithContentEncoding set Content-Encoding,
//     WithCacheControl set Cache-Control,
//     WithExpires set Expires,
//     WithMeta set meta header(s),
//     WithContentSHA256 set Content-Sha256,
//     WithContentMD5 set Content-MD5
//     WithExpires set Expires,
//     WithServerSideEncryptionCustomer set server side encryption options
//     WithACL WithACLGrantFullControl WithACLGrantRead WithACLGrantReadAcp WithACLGrantWrite WithACLGrantWriteAcp set object acl
//
// Deprecated: use CreateMultipartUpload of ClientV2 instead
func (bkt *Bucket) CreateMultipartUpload(ctx context.Context, objectKey string, options ...Option) (*CreateMultipartUploadOutput, error) {
	if err := isValidKey(objectKey); err != nil {
		return nil, err
	}

	res, err := bkt.client.newBuilder(bkt.name, objectKey, options...).
		WithQuery("uploads", "").
		Request(ctx, http.MethodPost, nil, bkt.client.roundTripper(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Close()

	var upload multipartUpload
	if err = marshalOutput(res.RequestInfo().RequestID, res.Body, &upload); err != nil {
		return nil, err
	}

	return &CreateMultipartUploadOutput{
		RequestInfo:          res.RequestInfo(),
		Bucket:               upload.Bucket,
		Key:                  upload.Key,
		UploadID:             upload.UploadID,
		SSECustomerAlgorithm: res.Header.Get(HeaderSSECustomerAlgorithm),
		SSECustomerKeyMD5:    res.Header.Get(HeaderSSECustomerKeyMD5),
	}, nil
}

// CreateMultipartUploadV2 create a multipart upload operation
func (cli *ClientV2) CreateMultipartUploadV2(
	ctx context.Context,
	input *CreateMultipartUploadV2Input) (*CreateMultipartUploadV2Output, error) {
	if err := IsValidBucketName(input.Bucket); err != nil {
		return nil, err
	}
	if err := isValidKey(input.Key); err != nil {
		return nil, err
	}

	res, err := cli.newBuilder(input.Bucket, input.Key).
		WithQuery("uploads", "").
		WithParams(*input).
		WithRetry(nil, ServerErrorClassifier{}).
		Request(ctx, http.MethodPost, nil, cli.roundTripper(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Close()

	var upload multipartUpload
	if err = marshalOutput(res.RequestInfo().RequestID, res.Body, &upload); err != nil {
		return nil, err
	}

	return &CreateMultipartUploadV2Output{
		RequestInfo:   res.RequestInfo(),
		Bucket:        upload.Bucket,
		Key:           upload.Key,
		UploadID:      upload.UploadID,
		SSECAlgorithm: res.Header.Get(HeaderSSECustomerAlgorithm),
		SSECKeyMD5:    res.Header.Get(HeaderSSECustomerKeyMD5),
		EncodingType:  res.Header.Get(HeaderContentEncoding),
	}, nil
}

// UploadPart upload a part for a multipart upload operation
// input: the parameters, some fields is required, e.g. Key, UploadID, PartNumber and PartNumber
//
// If uploading 'Content' with known Content-Length, please add option tos.WithContentLength
//
// Deprecated: use UploadPart of ClientV2 instead
func (bkt *Bucket) UploadPart(ctx context.Context, input *UploadPartInput, options ...Option) (*UploadPartOutput, error) {
	if err := isValidKey(input.Key); err != nil {
		return nil, err
	}

	res, err := bkt.client.newBuilder(bkt.name, input.Key, options...).
		WithQuery("uploadId", input.UploadID).
		WithQuery("partNumber", strconv.Itoa(input.PartNumber)).
		Request(ctx, http.MethodPut, input.Content, bkt.client.roundTripper(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Close()

	return &UploadPartOutput{
		RequestInfo:          res.RequestInfo(),
		PartNumber:           input.PartNumber,
		ETag:                 res.Header.Get(HeaderETag),
		SSECustomerAlgorithm: res.Header.Get(HeaderSSECustomerAlgorithm),
		SSECustomerKeyMD5:    res.Header.Get(HeaderSSECustomerKeyMD5),
	}, nil
}

// UploadPartV2 upload a part for a multipart upload operation
func (cli *ClientV2) UploadPartV2(ctx context.Context, input *UploadPartV2Input) (*UploadPartV2Output, error) {
	if err := isValidNames(input.Bucket, input.Key); err != nil {
		return nil, err
	}
	var (
		checker       hash.Hash64
		content       = input.Content
		contentLength = input.ContentLength
	)

	if input == nil || input.PartNumber == 0 || input.UploadID == "" {
		return nil, InputInvalidClientError
	}

	if contentLength == 0 {
		contentLength = tryResolveLength(content)
	}
	if cli.enableCRC {
		checker = NewCRC(DefaultCrcTable(), 0)
	}
	var (
		onRetry    func(req *Request) = nil
		classifier classifier
	)
	if content != nil {
		content = wrapReader(content, contentLength, input.DataTransferListener, input.RateLimiter, checker)
	}
	classifier = StatusCodeClassifier{}
	if seeker, ok := content.(io.Seeker); ok {
		start, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		onRetry = func(req *Request) {
			// PutObject/UploadPartV2 can be treated as an idempotent semantics if the request message body
			// supports a reset operation. e.g. the request message body is a string,
			// a local file handle, binary data in memory
			if seeker, ok := req.Content.(io.Seeker); ok {
				seeker.Seek(start, io.SeekStart)
			}
		}
	}
	if onRetry == nil {
		classifier = ServerErrorClassifier{}
	}
	res, err := cli.newBuilder(input.Bucket, input.Key).
		WithParams(*input).
		WithContentLength(input.ContentLength).
		WithRetry(onRetry, classifier).
		Request(ctx, http.MethodPut, content, cli.roundTripper(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Close()
	if err = checkCrc64(res, checker); err != nil {
		return nil, err
	}
	checksum, _ := strconv.ParseUint(res.Header.Get(HeaderHashCrc64ecma), 10, 64)
	return &UploadPartV2Output{
		RequestInfo:   res.RequestInfo(),
		PartNumber:    input.PartNumber,
		ETag:          res.Header.Get(HeaderETag),
		SSECAlgorithm: res.Header.Get(HeaderSSECustomerAlgorithm),
		SSECKeyMD5:    res.Header.Get(HeaderSSECustomerKeyMD5),
		HashCrc64ecma: checksum,
	}, nil
}

// UploadPartFromFile upload a part for a multipart upload operation from file
func (cli *ClientV2) UploadPartFromFile(ctx context.Context, input *UploadPartFromFileInput) (*UploadPartFromFileOutput, error) {
	file, err := os.Open(input.FilePath)
	if err != nil {
		return nil, err
	}
	_, err = file.Seek(int64(input.Offset), io.SeekStart)

	if err != nil {
		return nil, err
	}
	output, err := cli.UploadPartV2(ctx, &UploadPartV2Input{
		UploadPartBasicInput: input.UploadPartBasicInput,
		Content:              file,
		ContentLength:        input.PartSize,
	})
	if err != nil {
		return nil, err
	}
	return &UploadPartFromFileOutput{*output}, nil
}

// CompleteMultipartUpload complete a multipart upload operation
//   input: input.Key the object name,
//     input.UploadID the uploadID got from CreateMultipartUpload
//     input.UploadedParts upload part output got from UploadPart or UploadPartCopy
//
// Deprecated: use CompleteMultipartUpload of ClientV2 instead
func (bkt *Bucket) CompleteMultipartUpload(ctx context.Context, input *CompleteMultipartUploadInput, options ...Option) (*CompleteMultipartUploadOutput, error) {
	if err := isValidKey(input.Key); err != nil {
		return nil, err
	}
	multipart := partsToComplete{Parts: make(uploadedParts, 0, len(input.UploadedParts))}
	for _, p := range input.UploadedParts {
		multipart.Parts = append(multipart.Parts, p.uploadedPart())
	}

	sort.Sort(multipart.Parts)
	data, err := json.Marshal(&multipart)
	if err != nil {
		return nil, fmt.Errorf("tos: marshal uploadParts err: %s", err.Error())
	}

	res, err := bkt.client.newBuilder(bkt.name, input.Key, options...).
		WithQuery("uploadId", input.UploadID).
		Request(ctx, http.MethodPost, bytes.NewReader(data), bkt.client.roundTripper(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Close()

	return &CompleteMultipartUploadOutput{
		RequestInfo: res.RequestInfo(),
		VersionID:   res.Header.Get(HeaderVersionID),
	}, nil
}

// CompleteMultipartUploadV2 complete a multipart upload operation
func (cli *ClientV2) CompleteMultipartUploadV2(
	ctx context.Context, input *CompleteMultipartUploadV2Input) (*CompleteMultipartUploadV2Output, error) {

	if err := isValidNames(input.Bucket, input.Key); err != nil {
		return nil, err
	}
	multipart := partsToComplete{Parts: make(uploadedParts, 0, len(input.Parts))}
	for _, p := range input.Parts {
		multipart.Parts = append(multipart.Parts, p.uploadedPart())
	}

	sort.Sort(multipart.Parts)
	data, err := json.Marshal(&multipart)
	if err != nil {
		return nil, newTosClientError("tos: marshal uploadParts", err)
	}

	res, err := cli.newBuilder(input.Bucket, input.Key).
		WithParams(*input).
		WithRetry(nil, ServerErrorClassifier{}).
		Request(ctx, http.MethodPost, bytes.NewReader(data), cli.roundTripper(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Close()

	crc64, _ := strconv.ParseUint(res.Header.Get(HeaderHashCrc64ecma), 10, 64)
	output := &CompleteMultipartUploadV2Output{
		RequestInfo:   res.RequestInfo(),
		VersionID:     res.Header.Get(HeaderVersionID),
		HashCrc64ecma: crc64,
	}
	if err = marshalOutput(output.RequestID, res.Body, &output); err != nil {
		return nil, err
	}
	return output, nil
}

// AbortMultipartUpload abort a multipart upload operation
//
// Deprecated: use AbortMultipartUpload of ClientV2 instead
func (bkt *Bucket) AbortMultipartUpload(ctx context.Context, input *AbortMultipartUploadInput, options ...Option) (*AbortMultipartUploadOutput, error) {
	if err := isValidKey(input.Key); err != nil {
		return nil, err
	}
	res, err := bkt.client.newBuilder(bkt.name, input.Key, options...).
		WithQuery("uploadId", input.UploadID).
		Request(ctx, http.MethodDelete, nil, bkt.client.roundTripper(http.StatusNoContent))
	if err != nil {
		return nil, err
	}
	defer res.Close()

	return &AbortMultipartUploadOutput{RequestInfo: res.RequestInfo()}, nil
}

// AbortMultipartUpload abort a multipart upload operation
func (cli *ClientV2) AbortMultipartUpload(ctx context.Context, input *AbortMultipartUploadInput) (*AbortMultipartUploadOutput, error) {
	if err := isValidNames(input.Bucket, input.Key); err != nil {
		return nil, err
	}
	res, err := cli.newBuilder(input.Bucket, input.Key).
		WithParams(*input).
		WithRetry(nil, ServerErrorClassifier{}).
		Request(ctx, http.MethodDelete, nil, cli.roundTripper(http.StatusNoContent))
	if err != nil {
		return nil, err
	}
	defer res.Close()
	return &AbortMultipartUploadOutput{RequestInfo: res.RequestInfo()}, nil
}

// ListUploadedParts List Uploaded Parts
//   objectKey: the object name
//   input: key, uploadID and other parameters
//
// Deprecated: use ListParts of ClientV2 instead
func (bkt *Bucket) ListUploadedParts(ctx context.Context, input *ListUploadedPartsInput, options ...Option) (*ListUploadedPartsOutput, error) {
	if err := isValidKey(input.Key); err != nil {
		return nil, err
	}

	res, err := bkt.client.newBuilder(bkt.name, input.Key, options...).
		WithQuery("uploadId", input.UploadID).
		WithQuery("max-parts", strconv.Itoa(input.MaxParts)).
		WithQuery("part-number-marker", strconv.Itoa(input.PartNumberMarker)).
		Request(ctx, http.MethodGet, nil, bkt.client.roundTripper(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Close()

	output := ListUploadedPartsOutput{RequestInfo: res.RequestInfo()}
	if err = marshalOutput(output.RequestID, res.Body, &output); err != nil {
		return nil, err
	}
	return &output, nil
}

// ListParts List Uploaded Parts
func (cli *ClientV2) ListParts(ctx context.Context, input *ListPartsInput) (*ListPartsOutput, error) {
	if err := isValidNames(input.Bucket, input.Key); err != nil {
		return nil, err
	}
	res, err := cli.newBuilder(input.Bucket, input.Key).
		WithParams(*input).
		Request(ctx, http.MethodGet, nil, cli.roundTripper(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Close()

	output := ListPartsOutput{RequestInfo: res.RequestInfo()}
	if err = marshalOutput(output.RequestID, res.Body, &output); err != nil {
		return nil, err
	}
	return &output, nil
}

// ListMultipartUploads list multipart uploads
//
// Deprecated: use ListMultipartUploads of ClientV2 instead
func (bkt *Bucket) ListMultipartUploads(ctx context.Context, input *ListMultipartUploadsInput, options ...Option) (*ListMultipartUploadsOutput, error) {
	res, err := bkt.client.newBuilder(bkt.name, "", options...).
		WithQuery("uploads", "").
		WithQuery("prefix", input.Prefix).
		WithQuery("delimiter", input.Delimiter).
		WithQuery("key-marker", input.KeyMarker).
		WithQuery("upload-id-marker", input.UploadIDMarker).
		WithQuery("max-uploads", strconv.Itoa(input.MaxUploads)).
		Request(ctx, http.MethodGet, nil, bkt.client.roundTripper(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Close()

	output := ListMultipartUploadsOutput{RequestInfo: res.RequestInfo()}
	if err = marshalOutput(output.RequestID, res.Body, &output); err != nil {
		return nil, err
	}
	return &output, nil
}

// ListMultipartUploadsV2 list multipart uploads
func (cli *ClientV2) ListMultipartUploadsV2(
	ctx context.Context,
	input *ListMultipartUploadsV2Input) (*ListMultipartUploadsV2Output, error) {
	if err := IsValidBucketName(input.Bucket); err != nil {
		return nil, err
	}
	res, err := cli.newBuilder(input.Bucket, "").
		WithQuery("uploads", "").
		WithParams(*input).
		Request(ctx, http.MethodGet, nil, cli.roundTripper(http.StatusOK))
	if err != nil {
		return nil, err
	}
	defer res.Close()

	output := ListMultipartUploadsV2Output{RequestInfo: res.RequestInfo()}
	if err = marshalOutput(output.RequestID, res.Body, &output); err != nil {
		return nil, err
	}
	return &output, nil
}
