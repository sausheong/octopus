#import <Cocoa/Cocoa.h>

extern void octopusWillQuit(void);

@interface OctopusMenuDelegate : NSObject <NSApplicationDelegate>
@property(nonatomic, strong) NSStatusItem *statusItem;
@property(nonatomic, copy) NSString *settingsURL;
@end

@implementation OctopusMenuDelegate

- (void)applicationDidFinishLaunching:(NSNotification *)notification {
    (void)notification;
    self.statusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
    NSString *iconPath = [[NSBundle mainBundle] pathForResource:@"Octopus" ofType:@"png"];
    NSImage *icon = iconPath == nil ? nil : [[NSImage alloc] initWithContentsOfFile:iconPath];
    if (icon != nil) {
        icon.template = YES;
        icon.size = NSMakeSize(18.0, 18.0);
        self.statusItem.button.image = icon;
        self.statusItem.button.imagePosition = NSImageOnly;
        self.statusItem.button.imageScaling = NSImageScaleProportionallyDown;
    }
    self.statusItem.button.toolTip = @"Octopus";

    NSMenu *menu = [[NSMenu alloc] initWithTitle:@"Octopus"];
    NSMenuItem *settings = [[NSMenuItem alloc] initWithTitle:@"Settings…"
                                                       action:@selector(openSettings:)
                                                keyEquivalent:@","];
    settings.target = self;
    [menu addItem:settings];

    NSMenuItem *quit = [[NSMenuItem alloc] initWithTitle:@"Quit Octopus"
                                                   action:@selector(quitOctopus:)
                                            keyEquivalent:@"q"];
    quit.target = self;
    [menu addItem:quit];
    self.statusItem.menu = menu;
}

- (void)openSettings:(id)sender {
    (void)sender;
    NSURL *url = [NSURL URLWithString:self.settingsURL];
    if (url != nil) {
        [[NSWorkspace sharedWorkspace] openURL:url];
    }
}

- (void)quitOctopus:(id)sender {
    (void)sender;
    octopusWillQuit();
    [NSApp terminate:nil];
}

@end

static OctopusMenuDelegate *octopusDelegate;

void octopus_run(const char *settings_url) {
    @autoreleasepool {
        NSApplication *application = [NSApplication sharedApplication];
        [application setActivationPolicy:NSApplicationActivationPolicyAccessory];
        octopusDelegate = [[OctopusMenuDelegate alloc] init];
        octopusDelegate.settingsURL = [NSString stringWithUTF8String:settings_url];
        application.delegate = octopusDelegate;
        [application run];
    }
}
