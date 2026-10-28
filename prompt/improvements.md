Changes in the docs:
 1- /capture vs /validate and /mutate: i don't like the document structure about the difference between the capture feature with admission-tap/capture vs the mutate and validate feature with admission-tap=enabled. Separate the docs: Have clear instructions about how to capture and what capture does, and have instructions about how to mudate and validate and what this does. Also put in a different file other sections that are not related to the capture topic and the validate and mutate topic.
 1a- The capture section shoud have how to enable and disable capture section like it does, state the difference between the other option, show a record format and the relevant info around capture.
 1b- The validate and mutate section should have a similar approach as 1a. but now focused in validate and mutate.
 1c- create a sepate file or put it in another file the Redaction, Where records go sections.

 2- In the how it works file
 2a- Connect the three webhook configuations with the updated sections in 1a and 1b, so each object in this file can have an extended description in the other files/sections

 3- in README.md
 Explain that capture does briefly and that you can turn it off and have the mutate and validate webhook still logging data. Also separate the step by step in two, one for capturing and one for mutate and validate. Currently when users read it it seems that they are only doing the capture. Keep the breavity, be concise and succint. This is for humans to understand
