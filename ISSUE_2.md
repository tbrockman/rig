16:31:20  (qt.qpa.services) Failed to register with host portal QDBusError("org.freedesktop.portal.Error.Failed", "Could not register app ID: App info not found for 'org.freecad.FreeCAD'")
16:32:42  Running the Python command 'KCL_Open' failed:
Traceback (most recent call last):
  File "/home/theo/.local/share/FreeCAD/v1-1/Mod/KCL/kclcad_wb/commands.py", line 111, in Activated
    code, params = _panels_for(kcl)
  File "/home/theo/.local/share/FreeCAD/v1-1/Mod/KCL/kclcad_wb/commands.py", line 80, in _panels_for
    main.addDockWidget(0x2, dock)  # Qt.RightDockWidgetArea

'PySide6.QtWidgets.QMainWindow.addDockWidget' called with wrong argument types:
  PySide6.QtWidgets.QMainWindow.addDockWidget(int, QDockWidget)
Supported signatures:
  PySide6.QtWidgets.QMainWindow.addDockWidget(area: PySide6.QtCore.Qt.DockWidgetArea, dockwidget: PySide6.QtWidgets.QDockWidget, /)
  PySide6.QtWidgets.QMainWindow.addDockWidget(area: PySide6.QtCore.Qt.DockWidgetArea, dockwidget: PySide6.QtWidgets.QDockWidget, orientation: PySide6.QtCore.Qt.Orientation, /)
