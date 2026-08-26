theo@theo-ny-desktop-ubuntu:~/dev/kclcad$ nix develop
kcl-freecad shell: rustc 1.97.1, freecad FreeCAD 1.1.3 Revision: Unknown, kicad-cli 10.0.5
(nix:kcl-freecad-env) theo@theo-ny-desktop-ubuntu:~/dev/kclcad$ echo $PYTHONPATH
/nix/store/kflcdgvqa7cj6c73cr7pkk84zq1mxq59-freecad-1.1.3/lib:/nix/store/3n4qphl9s728sz8frmpqqrv9b1m87g68-python3-3.14.7/lib/python3.14/site-packages:/nix/store/809x02ix874bchaw7izlaznhhn5ss5ww-python3.14-pip-26.1.2/lib/python3.14/site-packages
(nix:kcl-freecad-env) theo@theo-ny-desktop-ubuntu:~/dev/kclcad$ export PYTHONPATH="$PWD/target/pylib:$PWD/freecad:$PYTHONPATH"
(nix:kcl-freecad-env) theo@theo-ny-desktop-ubuntu:~/dev/kclcad$ freecad
FreeCAD 1.1.3, Libs: 1.1.3RUnknown
(C) 2001-1980 FreeCAD contributors
FreeCAD is free and open-source software licensed under the terms of LGPL2+ license.

QOpenGLWidget is not supported on this platform.
(qt.qpa.services) Failed to register with host portal QDBusError("org.freedesktop.portal.Error.Failed", "Could not register app ID: App info not found for 'org.freecad.FreeCAD'")
During initialization the error "name '__file__' is not defined" occurred in /home/theo/.local/share/FreeCAD/Mod/KCL/InitGui.py
Look into the log file for further information
QRhiGles2: Failed to create temporary context
QRhiGles2: Failed to create context
Failed to create QRhi for QBackingStoreRhiSupport
QOpenGLWidget: Failed to create context
Gtk-Message: 16:12:24.811: Failed to load module "canberra-gtk-module"
Migrating config from /home/theo/.config/FreeCAD to /home/theo/.config/FreeCAD/v1-1
Migrating config from /home/theo/.local/share/FreeCAD to /home/theo/.local/share/FreeCAD/v1-1
(nix:kcl-freecad-env) theo@theo-ny-desktop-ubuntu:~/dev/kclcad$ FreeCAD 1.1.3, Libs: 1.1.3RUnknown
(C) 2001-1980 FreeCAD contributors
FreeCAD is free and open-source software licensed under the terms of LGPL2+ license.

QOpenGLWidget is not supported on this platform.
(qt.qpa.services) Failed to register with host portal QDBusError("org.freedesktop.portal.Error.Failed", "Could not register app ID: App info not found for 'org.freecad.FreeCAD'")
Gtk-Message: 16:12:38.520: Failed to load module "canberra-gtk-module"
During initialization the error "name '__file__' is not defined" occurred in /home/theo/.local/share/FreeCAD/v1-1/Mod/KCL/InitGui.py
Look into the log file for further information
QRhiGles2: Failed to create temporary context
QRhiGles2: Failed to create context
Failed to create QRhi for QBackingStoreRhiSupport
QOpenGLWidget: Failed to create context
