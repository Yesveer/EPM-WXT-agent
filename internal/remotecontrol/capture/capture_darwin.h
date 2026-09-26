#ifndef WXT_CAPTURE_DARWIN_H
#define WXT_CAPTURE_DARWIN_H

#include <stdint.h>

// Result codes shared with the Go side.
enum {
	WXT_OK = 0,
	WXT_ERR_PERMISSION = 1, // Screen Recording not granted
	WXT_ERR_NO_DISPLAY = 2,
	WXT_ERR_STREAM = 3,     // ScreenCaptureKit refused to start
	WXT_ERR_TIMEOUT = 4,    // no frame arrived in time
	WXT_ERR_STOPPED = 5,    // the stream died (display change, logout)
};

// wxt_capture_start begins streaming the main display and reports its size in
// POINTS. errbuf receives a human-readable reason on failure.
int wxt_capture_start(int *width, int *height, char *errbuf, int errlen);

// wxt_capture_copy blocks for up to timeout_ms for a frame at least as new as
// the previous one, then copies width*height*4 BGRA bytes into dst.
int wxt_capture_copy(void *dst, int width, int height, int timeout_ms);

// wxt_capture_size reports the current display size in points.
void wxt_capture_size(int *width, int *height);

void wxt_capture_stop(void);

#endif
