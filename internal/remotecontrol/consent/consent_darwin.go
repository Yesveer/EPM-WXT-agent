//go:build darwin

package consent

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>

// wxt_ask shows a system alert and returns the response flags.
//
// CFUserNotification is the API designed for exactly this situation: a
// background process that needs the logged-in user's attention. Unlike an
// AppleScript "display dialog" it needs no Automation TCC grant and no
// foreground application, and unlike NSAlert it does not require a run loop.
// It also takes the timeout directly, so an unanswered prompt cleans itself
// up instead of sitting on the screen forever.
static int wxt_ask(double timeout, const char *title, const char *body,
                   const char *allowBtn, const char *denyBtn, int *response) {
	CFStringRef cfTitle = CFStringCreateWithCString(NULL, title, kCFStringEncodingUTF8);
	CFStringRef cfBody  = CFStringCreateWithCString(NULL, body,  kCFStringEncodingUTF8);
	CFStringRef cfAllow = CFStringCreateWithCString(NULL, allowBtn, kCFStringEncodingUTF8);
	CFStringRef cfDeny  = CFStringCreateWithCString(NULL, denyBtn,  kCFStringEncodingUTF8);

	CFOptionFlags flags = 0;
	// Deny is the DEFAULT button: if the user hits return to dismiss a dialog
	// they did not read, the safe answer is the one that happens.
	SInt32 rc = CFUserNotificationDisplayAlert(
		timeout,
		kCFUserNotificationCautionAlertLevel,
		NULL, NULL, NULL,
		cfTitle, cfBody,
		cfDeny,   // default button
		cfAllow,  // alternate button
		NULL,
		&flags);

	if (cfTitle) CFRelease(cfTitle);
	if (cfBody)  CFRelease(cfBody);
	if (cfAllow) CFRelease(cfAllow);
	if (cfDeny)  CFRelease(cfDeny);

	if (rc != 0) return 0;              // could not display at all
	*response = (int)(flags & 0x3);
	return 1;
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

// CFUserNotification response codes.
const (
	respDefault   = 0 // our Deny button
	respAlternate = 1 // our Allow button
	respCancel    = 3 // timed out or dismissed
)

func ask(req Request) (Decision, error) {
	title := C.CString(promptTitle)
	body := C.CString(message(req))
	allow := C.CString("Allow")
	deny := C.CString("Deny")
	defer func() {
		C.free(unsafe.Pointer(title))
		C.free(unsafe.Pointer(body))
		C.free(unsafe.Pointer(allow))
		C.free(unsafe.Pointer(deny))
	}()

	var response C.int
	ok := C.wxt_ask(C.double(req.Timeout.Seconds()), title, body, allow, deny, &response)
	if ok == 0 {
		// No window server, no logged-in user, or the alert was refused. With
		// no way to ask, the answer is no.
		return Denied, errors.New("could not display the consent prompt")
	}

	switch int(response) {
	case respAlternate:
		return Approved, nil
	case respCancel:
		return TimedOut, nil
	case respDefault:
		return Denied, nil
	default:
		return Denied, nil
	}
}
