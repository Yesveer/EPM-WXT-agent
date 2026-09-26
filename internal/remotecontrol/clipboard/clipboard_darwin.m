// macOS clipboard access via NSPasteboard.
//
// AppKit is linked here even though the helper draws no windows: NSPasteboard
// lives in AppKit and works fine without an NSApplication run loop, which is
// what lets a background process take part in copy and paste at all.

#import <AppKit/AppKit.h>
#include <stdlib.h>
#include <string.h>

// wxt_clip_change_count returns NSPasteboard's monotonically increasing
// counter. Polling this integer is how a local copy is noticed without
// re-reading (and re-allocating) the pasteboard contents every tick.
long wxt_clip_change_count(void) {
	@autoreleasepool {
		return (long)[[NSPasteboard generalPasteboard] changeCount];
	}
}

// wxt_clip_read returns a malloc'd UTF-8 copy of the pasteboard text, or NULL
// when the pasteboard holds something that is not text. The caller frees it.
char *wxt_clip_read(void) {
	@autoreleasepool {
		NSPasteboard *pb = [NSPasteboard generalPasteboard];
		NSString *s = [pb stringForType:NSPasteboardTypeString];
		if (!s) return NULL;
		const char *utf8 = [s UTF8String];
		if (!utf8) return NULL;
		return strdup(utf8);
	}
}

// wxt_clip_write replaces the pasteboard contents. Returns 1 on success.
int wxt_clip_write(const char *utf8) {
	@autoreleasepool {
		NSString *s = [NSString stringWithUTF8String:utf8];
		if (!s) return 0;
		NSPasteboard *pb = [NSPasteboard generalPasteboard];
		// clearContents is mandatory before setString: without it the write is
		// rejected and the old contents silently survive.
		[pb clearContents];
		return [pb setString:s forType:NSPasteboardTypeString] ? 1 : 0;
	}
}
