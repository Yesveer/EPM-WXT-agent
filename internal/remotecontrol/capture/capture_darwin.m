// macOS screen capture via ScreenCaptureKit.
//
// The obvious choice would have been CGDisplayCreateImage — three lines of C —
// but Apple did not merely deprecate it: as of the macOS 15 SDK it is marked
// unavailable and will not compile. ScreenCaptureKit is the only supported
// route, and it is a streaming API rather than a screenshot one, so this file
// keeps a running SCStream and hands the newest frame to Go on demand.
//
// Streaming turns out to be the better fit anyway: SCK composites the real
// cursor for us (showsCursor), scales to the requested size on the GPU, and
// only delivers a buffer when something actually changed.

#import <Foundation/Foundation.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>

#include <string.h>
#include "capture_darwin.h"

static void wxt_set_err(char *errbuf, int errlen, NSString *msg) {
	if (!errbuf || errlen <= 0) return;
	const char *s = [msg UTF8String];
	if (!s) s = "unknown error";
	strncpy(errbuf, s, (size_t)errlen - 1);
	errbuf[errlen - 1] = '\0';
}

@interface WxtCapture : NSObject <SCStreamOutput, SCStreamDelegate>
@property (nonatomic, strong) SCStream *stream;
@property (nonatomic, strong) NSCondition *cond;
@property (nonatomic, assign) uint8_t *frame;
@property (nonatomic, assign) int width;
@property (nonatomic, assign) int height;
@property (nonatomic, assign) uint64_t seq;     // bumped on every new frame
@property (nonatomic, assign) BOOL stopped;
@end

@implementation WxtCapture

- (void)dealloc {
	if (_frame) { free(_frame); _frame = NULL; }
}

// The stream stopping is not recoverable from here — the user logged out, the
// display went away, or the TCC grant was revoked mid-session. Record it so
// the next copy fails loudly instead of returning a frozen last frame forever.
- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
	[self.cond lock];
	self.stopped = YES;
	[self.cond broadcast];
	[self.cond unlock];
}

- (void)stream:(SCStream *)stream
	didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
				   ofType:(SCStreamOutputType)type {
	if (type != SCStreamOutputTypeScreen) return;
	if (!CMSampleBufferIsValid(sampleBuffer)) return;

	CVImageBufferRef px = CMSampleBufferGetImageBuffer(sampleBuffer);
	if (!px) return;

	CVPixelBufferLockBaseAddress(px, kCVPixelBufferLock_ReadOnly);
	size_t w = CVPixelBufferGetWidth(px);
	size_t h = CVPixelBufferGetHeight(px);
	size_t stride = CVPixelBufferGetBytesPerRow(px);
	const uint8_t *src = (const uint8_t *)CVPixelBufferGetBaseAddress(px);

	if (src) {
		[self.cond lock];
		if (self.frame && (int)w == self.width && (int)h == self.height) {
			// CVPixelBuffer rows are padded to a hardware-friendly stride, so
			// a single memcpy of the whole plane would import the padding as
			// pixels and shear the image. Copy row by row.
			size_t rowBytes = (size_t)self.width * 4;
			for (size_t y = 0; y < h; y++) {
				memcpy(self.frame + y * rowBytes, src + y * stride, rowBytes);
			}
			self.seq++;
			[self.cond broadcast];
		}
		[self.cond unlock];
	}

	CVPixelBufferUnlockBaseAddress(px, kCVPixelBufferLock_ReadOnly);
}

@end

static WxtCapture *g_cap = nil;

int wxt_capture_start(int *width, int *height, char *errbuf, int errlen) {
	@autoreleasepool {
		if (g_cap) { wxt_capture_stop(); }

		__block SCShareableContent *content = nil;
		__block NSError *contentErr = nil;
		dispatch_semaphore_t sem = dispatch_semaphore_create(0);

		// This call is the TCC gate: without Screen Recording it fails rather
		// than returning an empty list, so its error is how permission denial
		// is detected.
		[SCShareableContent getShareableContentExcludingDesktopWindows:NO
												   onScreenWindowsOnly:NO
													 completionHandler:^(SCShareableContent *c, NSError *e) {
			content = c;
			contentErr = e;
			dispatch_semaphore_signal(sem);
		}];

		if (dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 15 * NSEC_PER_SEC)) != 0) {
			wxt_set_err(errbuf, errlen, @"timed out querying shareable content");
			return WXT_ERR_TIMEOUT;
		}
		if (contentErr || !content) {
			wxt_set_err(errbuf, errlen,
				contentErr ? contentErr.localizedDescription : @"no shareable content");
			return WXT_ERR_PERMISSION;
		}
		if (content.displays.count == 0) {
			wxt_set_err(errbuf, errlen, @"no displays available");
			return WXT_ERR_NO_DISPLAY;
		}

		SCDisplay *display = content.displays.firstObject;
		int w = (int)display.width;   // points, not backing pixels
		int h = (int)display.height;
		if (w <= 0 || h <= 0) {
			wxt_set_err(errbuf, errlen, @"display reported a zero size");
			return WXT_ERR_NO_DISPLAY;
		}

		WxtCapture *cap = [[WxtCapture alloc] init];
		cap.cond = [[NSCondition alloc] init];
		cap.width = w;
		cap.height = h;
		cap.frame = (uint8_t *)calloc((size_t)w * (size_t)h, 4);
		if (!cap.frame) {
			wxt_set_err(errbuf, errlen, @"out of memory allocating framebuffer");
			return WXT_ERR_STREAM;
		}

		SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:display
														  excludingWindows:@[]];

		SCStreamConfiguration *cfg = [[SCStreamConfiguration alloc] init];
		// Asking SCK for the point size makes it scale on the GPU. Capturing
		// at point rather than backing resolution halves the bytes on a Retina
		// display AND makes framebuffer coordinates identical to the ones
		// CGEvent expects for input, which removes every scale conversion.
		cfg.width = (size_t)w;
		cfg.height = (size_t)h;
		cfg.pixelFormat = kCVPixelFormatType_32BGRA;
		cfg.showsCursor = YES;
		cfg.queueDepth = 3;
		cfg.minimumFrameInterval = CMTimeMake(1, 30);
		cfg.scalesToFit = YES;

		NSError *streamErr = nil;
		SCStream *stream = [[SCStream alloc] initWithFilter:filter
											  configuration:cfg
												   delegate:cap];
		dispatch_queue_t q = dispatch_queue_create("io.wxt.agent.capture", DISPATCH_QUEUE_SERIAL);
		if (![stream addStreamOutput:cap
								type:SCStreamOutputTypeScreen
				  sampleHandlerQueue:q
							   error:&streamErr]) {
			wxt_set_err(errbuf, errlen,
				streamErr ? streamErr.localizedDescription : @"addStreamOutput failed");
			return WXT_ERR_STREAM;
		}

		__block NSError *startErr = nil;
		dispatch_semaphore_t startSem = dispatch_semaphore_create(0);
		[stream startCaptureWithCompletionHandler:^(NSError *e) {
			startErr = e;
			dispatch_semaphore_signal(startSem);
		}];
		if (dispatch_semaphore_wait(startSem, dispatch_time(DISPATCH_TIME_NOW, 15 * NSEC_PER_SEC)) != 0) {
			wxt_set_err(errbuf, errlen, @"timed out starting capture");
			return WXT_ERR_TIMEOUT;
		}
		if (startErr) {
			wxt_set_err(errbuf, errlen, startErr.localizedDescription);
			return WXT_ERR_STREAM;
		}

		cap.stream = stream;
		g_cap = cap;
		if (width) *width = w;
		if (height) *height = h;
		return WXT_OK;
	}
}

int wxt_capture_copy(void *dst, int width, int height, int timeout_ms) {
	WxtCapture *cap = g_cap;
	if (!cap) return WXT_ERR_STOPPED;
	if (width != cap.width || height != cap.height) return WXT_ERR_STOPPED;

	[cap.cond lock];
	if (cap.stopped) { [cap.cond unlock]; return WXT_ERR_STOPPED; }

	// Wait for a frame only if none has arrived yet. Once the stream is
	// running the newest frame is always good enough, and blocking for a
	// *newer* one would stall on a perfectly static desktop — which is the
	// normal state of a machine being talked about rather than typed on.
	if (cap.seq == 0) {
		NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:(double)timeout_ms / 1000.0];
		while (cap.seq == 0 && !cap.stopped) {
			if (![cap.cond waitUntilDate:deadline]) break;
		}
		if (cap.seq == 0) {
			BOOL stopped = cap.stopped;
			[cap.cond unlock];
			return stopped ? WXT_ERR_STOPPED : WXT_ERR_TIMEOUT;
		}
	}

	memcpy(dst, cap.frame, (size_t)width * (size_t)height * 4);
	[cap.cond unlock];
	return WXT_OK;
}

void wxt_capture_size(int *width, int *height) {
	WxtCapture *cap = g_cap;
	if (!cap) { if (width) *width = 0; if (height) *height = 0; return; }
	if (width) *width = cap.width;
	if (height) *height = cap.height;
}

void wxt_capture_stop(void) {
	WxtCapture *cap = g_cap;
	if (!cap) return;
	g_cap = nil;

	if (cap.stream) {
		dispatch_semaphore_t sem = dispatch_semaphore_create(0);
		[cap.stream stopCaptureWithCompletionHandler:^(NSError *e) {
			dispatch_semaphore_signal(sem);
		}];
		// Bounded wait: a hung stop must not wedge agent shutdown.
		dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC));
	}

	[cap.cond lock];
	cap.stopped = YES;
	[cap.cond broadcast];
	[cap.cond unlock];
}
